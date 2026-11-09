// macOS USB transport. Opens only the selected vendor AT interface; never seizes
// a device, resets it, changes its configuration or detaches ECM/audio drivers.
#include <CoreFoundation/CoreFoundation.h>
#include <CoreAudio/CoreAudio.h>
#include <AudioToolbox/AudioToolbox.h>
#include <IOKit/IOCFPlugIn.h>
#include <IOKit/usb/IOUSBLib.h>
#include <poll.h>
#include <pthread.h>
#include <signal.h>
#include <stdint.h>
#include <stdatomic.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

static _Atomic int running = 1;
static void stop(int sig) { (void)sig; running = 0; }
static CFStringRef audio_uid(AudioDeviceID device) {
    AudioObjectPropertyAddress p = {kAudioDevicePropertyDeviceUID, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
    CFStringRef uid = NULL; UInt32 size = sizeof(uid);
    if (AudioObjectGetPropertyData(device, &p, 0, NULL, &size, &uid)) return NULL;
    return uid;
}
static unsigned audio_channels(AudioDeviceID device, AudioObjectPropertyScope scope) {
    AudioObjectPropertyAddress p = {kAudioDevicePropertyStreamConfiguration, scope, kAudioObjectPropertyElementMain};
    UInt32 size = 0;
    if (AudioObjectGetPropertyDataSize(device, &p, 0, NULL, &size)) return 0;
    AudioBufferList *buffers = calloc(1, size); if (!buffers) return 0;
    unsigned channels = 0;
    if (!AudioObjectGetPropertyData(device, &p, 0, NULL, &size, buffers))
        for (UInt32 i = 0; i < buffers->mNumberBuffers; i++) channels += buffers->mBuffers[i].mNumberChannels;
    free(buffers); return channels;
}
static void json_string(const char *s) {
    putchar('"');
    for (; *s; s++) {
        unsigned char c = (unsigned char)*s;
        if (c == '"' || c == '\\') {putchar('\\'); putchar(c);}
        else if (c < 32) printf("\\u%04x", c);
        else putchar(c);
    }
    putchar('"');
}
static int audio_devices(void) {
    AudioObjectPropertyAddress p = {kAudioHardwarePropertyDevices, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
    UInt32 size = 0;
    if (AudioObjectGetPropertyDataSize(kAudioObjectSystemObject, &p, 0, NULL, &size)) return 1;
    AudioDeviceID *ids = calloc(1, size); if (!ids) return 1;
    if (AudioObjectGetPropertyData(kAudioObjectSystemObject, &p, 0, NULL, &size, ids)) {free(ids); return 1;}
    printf("["); int comma = 0;
    for (unsigned i = 0; i < size / sizeof(*ids); i++) {
        CFStringRef uid = audio_uid(ids[i]); if (!uid) continue;
        char value[2048];
        if (CFStringGetCString(uid, value, sizeof(value), kCFStringEncodingUTF8)) {
            printf("%s{\"uid\":", comma ? "," : ""); json_string(value);
            printf(",\"input\":%u,\"output\":%u}", audio_channels(ids[i], kAudioDevicePropertyScopeInput), audio_channels(ids[i], kAudioDevicePropertyScopeOutput)); comma = 1;
        }
        CFRelease(uid);
    }
    free(ids); printf("]\n"); return 0;
}
static uint64_t prop(io_registry_entry_t entry, CFStringRef key) {
    CFTypeRef value = IORegistryEntryCreateCFProperty(entry, key, kCFAllocatorDefault, 0);
    int64_t n = 0;
    if (value && CFGetTypeID(value) == CFNumberGetTypeID())
        CFNumberGetValue(value, kCFNumberSInt64Type, &n);
    if (value) CFRelease(value);
    return (uint64_t)n;
}
static void network_name(io_service_t entry, char *name, size_t size) {
    CFTypeRef value = IORegistryEntrySearchCFProperty(entry, kIOServicePlane, CFSTR("BSD Name"),
        kCFAllocatorDefault, kIORegistryIterateRecursively);
    name[0] = 0;
    if (value && CFGetTypeID(value) == CFStringGetTypeID())
        CFStringGetCString(value, name, (CFIndex)size, kCFStringEncodingUTF8);
    if (value) CFRelease(value);
    // The only accepted values are BSD interface identifiers.
    for (char *p = name; *p; p++) if (!((*p >= 'a' && *p <= 'z') || (*p >= '0' && *p <= '9'))) { name[0] = 0; break; }
}
static int devices(void) {
    io_iterator_t iter = 0;
    if (IOServiceGetMatchingServices(kIOMainPortDefault, IOServiceMatching("IOUSBHostDevice"), &iter)) return 1;
    printf("[");
    int comma = 0;
    io_service_t entry;
    while ((entry = IOIteratorNext(iter))) {
        if (prop(entry, CFSTR("idVendor")) == 0x2c7c && prop(entry, CFSTR("idProduct")) == 0x0125) {
            char network[32]; network_name(entry, network, sizeof(network));
            printf("%s{\"location\":\"%08llx\",\"session\":\"%llu\",\"network\":\"%s\"}", comma ? "," : "",
                   (unsigned long long)prop(entry, CFSTR("locationID")),
                   (unsigned long long)prop(entry, CFSTR("sessionID")), network);
            comma = 1;
        }
        IOObjectRelease(entry);
    }
    IOObjectRelease(iter);
    printf("]\n");
    return 0;
}
static int usb_string(io_service_t entry, CFStringRef key, char *value, size_t size) {
    CFTypeRef cf = IORegistryEntryCreateCFProperty(entry, key, kCFAllocatorDefault, 0);
    int ok = cf && CFGetTypeID(cf) == CFStringGetTypeID() && CFStringGetCString(cf, value, size, kCFStringEncodingUTF8);
    if (cf) CFRelease(cf);
    if (!ok || !value[0] || strchr(value, ':')) return 0;
    return 1;
}
typedef struct { char input[2048], output[2048]; uint64_t session; } AudioSelection;
static int select_audio(uint32_t location, AudioSelection *selected) {
    // Generic factory serials can produce identical CoreAudio UIDs. Refuse an
    // ambiguous pair even if an AT location was explicitly selected.
    io_iterator_t iter = 0; unsigned count = 0; int found = 0;
    if (IOServiceGetMatchingServices(kIOMainPortDefault, IOServiceMatching("IOUSBHostDevice"), &iter)) return 1;
    io_service_t entry;
    while ((entry = IOIteratorNext(iter))) {
        if (prop(entry, CFSTR("idVendor")) == 0x2c7c && prop(entry, CFSTR("idProduct")) == 0x0125) {
            count++;
            if (prop(entry, CFSTR("locationID")) == location) {
                char vendor[256], product[256], serial[256];
                if (usb_string(entry, CFSTR("USB Vendor Name"), vendor, sizeof(vendor)) &&
                    usb_string(entry, CFSTR("USB Product Name"), product, sizeof(product)) &&
                    usb_string(entry, CFSTR("USB Serial Number"), serial, sizeof(serial))) {
                    snprintf(selected->input, sizeof(selected->input), "AppleUSBAudioEngine:%s:%s:%s:7", vendor, product, serial);
                    snprintf(selected->output, sizeof(selected->output), "AppleUSBAudioEngine:%s:%s:%s:8", vendor, product, serial);
                    selected->session = prop(entry, CFSTR("sessionID")); found = 1;
                }
            }
        }
        IOObjectRelease(entry);
    }
    IOObjectRelease(iter);
    if (!found || count != 1) {fprintf(stderr, "need one unambiguous EC20 USB audio device\n"); return 2;}
    AudioObjectPropertyAddress p = {kAudioHardwarePropertyDevices, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
    UInt32 size = 0;
    if (AudioObjectGetPropertyDataSize(kAudioObjectSystemObject, &p, 0, NULL, &size)) return 3;
    AudioDeviceID *ids = calloc(1, size); if (!ids) return 3;
    if (AudioObjectGetPropertyData(kAudioObjectSystemObject, &p, 0, NULL, &size, ids)) {free(ids); return 3;}
    unsigned input = 0, output = 0;
    for (unsigned i = 0; i < size / sizeof(*ids); i++) {
        CFStringRef uid = audio_uid(ids[i]); if (!uid) continue;
        char value[2048];
        if (CFStringGetCString(uid, value, sizeof(value), kCFStringEncodingUTF8)) {
            AudioObjectPropertyAddress rate_prop = {kAudioDevicePropertyNominalSampleRate, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
            Float64 rate = 0; UInt32 bytes = sizeof(rate);
            if (!AudioObjectGetPropertyData(ids[i], &rate_prop, 0, NULL, &bytes, &rate) && rate == 8000) {
                if (!strcmp(value, selected->input) && audio_channels(ids[i], kAudioDevicePropertyScopeInput) == 1) input++;
                if (!strcmp(value, selected->output) && audio_channels(ids[i], kAudioDevicePropertyScopeOutput) == 1) output++;
            }
        }
        CFRelease(uid);
    }
    free(ids);
    if (input != 1 || output != 1) {fprintf(stderr, "EC20 8000 Hz mono audio pair unavailable\n"); return 4;}
    return 0;
}

// Audio callbacks never touch files or stdio. Bounded rings separate the real
// time callbacks from the Go media pipes. Playback underflow is silent.
#define PCM_RING 8000
typedef struct { unsigned char bytes[PCM_RING]; size_t start, length; pthread_mutex_t lock; } PCMRing;
typedef struct { PCMRing capture, playback; AudioQueueRef input, output; } PCMState;
static void ring_put(PCMRing *r, const void *data, size_t n) {
    pthread_mutex_lock(&r->lock);
    const unsigned char *b = data;
    for (size_t i = 0; i < n; i++) {
        if (r->length == PCM_RING) {r->start = (r->start + 2) % PCM_RING; r->length -= 2;}
        r->bytes[(r->start + r->length) % PCM_RING] = b[i]; r->length++;
    }
    pthread_mutex_unlock(&r->lock);
}
static size_t ring_get(PCMRing *r, void *data, size_t n) {
    pthread_mutex_lock(&r->lock);
    if (n > r->length) n = r->length;
    n &= ~(size_t)1;
    unsigned char *b = data;
    for (size_t i = 0; i < n; i++) b[i] = r->bytes[(r->start + i) % PCM_RING];
    r->start = (r->start + n) % PCM_RING; r->length -= n;
    pthread_mutex_unlock(&r->lock); return n;
}
static void capture_audio(void *user, AudioQueueRef queue, AudioQueueBufferRef buffer,
                          const AudioTimeStamp *time, UInt32 packets, const AudioStreamPacketDescription *descriptions) {
    (void)time; (void)packets; (void)descriptions;
    PCMState *state = user;
    if (!running) return;
    ring_put(&state->capture, buffer->mAudioData, buffer->mAudioDataByteSize);
    if (AudioQueueEnqueueBuffer(queue, buffer, 0, NULL)) running = 0;
}
static void play_audio(void *user, AudioQueueRef queue, AudioQueueBufferRef buffer) {
    PCMState *state = user;
    if (!running) return;
    size_t n = ring_get(&state->playback, buffer->mAudioData, 320);
    memset((unsigned char *)buffer->mAudioData + n, 0, 320 - n);
    buffer->mAudioDataByteSize = 320;
    if (AudioQueueEnqueueBuffer(queue, buffer, 0, NULL)) running = 0;
}
static void *write_capture(void *user) {
    PCMState *state = user;
    while (running) {
        unsigned char data[320]; size_t n = ring_get(&state->capture, data, sizeof(data));
        if (!n) {usleep(5000); continue;}
        size_t at = 0;
        while (at < n && running) {
            struct pollfd p = {STDOUT_FILENO, POLLOUT, 0};
            if (poll(&p, 1, 100) <= 0) continue;
            ssize_t written = write(STDOUT_FILENO, data + at, n - at);
            if (written <= 0) {running = 0; break;} at += written;
        }
    }
    return NULL;
}
static int audio_bridge(uint32_t location) {
    AudioSelection selected = {0}, after = {0};
    if (select_audio(location, &selected)) return 2;
    PCMState state = {0}; pthread_mutex_init(&state.capture.lock, NULL); pthread_mutex_init(&state.playback.lock, NULL);
    AudioStreamBasicDescription format = {.mSampleRate=8000, .mFormatID=kAudioFormatLinearPCM,
        .mFormatFlags=kLinearPCMFormatFlagIsSignedInteger | kAudioFormatFlagIsPacked,
        .mBytesPerPacket=2, .mFramesPerPacket=1, .mBytesPerFrame=2, .mChannelsPerFrame=1, .mBitsPerChannel=16};
    OSStatus err = AudioQueueNewInput(&format, capture_audio, &state, NULL, NULL, 0, &state.input);
    if (!err) err = AudioQueueNewOutput(&format, play_audio, &state, NULL, NULL, 0, &state.output);
    CFStringRef in = CFStringCreateWithCString(NULL, selected.input, kCFStringEncodingUTF8);
    CFStringRef out = CFStringCreateWithCString(NULL, selected.output, kCFStringEncodingUTF8);
    if (!err) err = AudioQueueSetProperty(state.input, kAudioQueueProperty_CurrentDevice, &in, sizeof(in));
    if (!err) err = AudioQueueSetProperty(state.output, kAudioQueueProperty_CurrentDevice, &out, sizeof(out));
    CFRelease(in); CFRelease(out);
    for (int i = 0; i < 3 && !err; i++) {
        AudioQueueBufferRef capture = NULL, playback = NULL;
        err = AudioQueueAllocateBuffer(state.input, 320, &capture);
        if (!err) err = AudioQueueEnqueueBuffer(state.input, capture, 0, NULL);
        if (!err) err = AudioQueueAllocateBuffer(state.output, 320, &playback);
        if (!err) {memset(playback->mAudioData, 0, 320); playback->mAudioDataByteSize = 320; err = AudioQueueEnqueueBuffer(state.output, playback, 0, NULL);}
    }
    if (!err && (select_audio(location, &after) || selected.session != after.session)) err = kAudioHardwareBadDeviceError;
    signal(SIGINT, stop); signal(SIGTERM, stop); signal(SIGPIPE, stop);
    if (!err) err = AudioQueueStart(state.input, NULL);
    if (!err) err = AudioQueueStart(state.output, NULL);
    pthread_t writer; int writer_started = 0;
    if (!err) {writer_started = pthread_create(&writer, NULL, write_capture, &state) == 0; if (!writer_started) err = -1;}
    while (!err && running) {
        struct pollfd p = {STDIN_FILENO, POLLIN, 0};
        if (poll(&p, 1, 100) <= 0) continue;
        unsigned char data[320]; ssize_t n = read(STDIN_FILENO, data, sizeof(data));
        if (n <= 0) break;
        ring_put(&state.playback, data, (size_t)n);
    }
    running = 0;
    if (state.input) AudioQueueDispose(state.input, true);
    if (state.output) AudioQueueDispose(state.output, true);
    if (writer_started) pthread_join(writer, NULL);
    pthread_mutex_destroy(&state.capture.lock); pthread_mutex_destroy(&state.playback.lock);
    if (err) fprintf(stderr, "CoreAudio stream unavailable (%d); check microphone permission\n", (int)err);
    return err ? 5 : 0;
}
static int bridge(uint32_t location) {
    io_iterator_t iter = 0;
    if (IOServiceGetMatchingServices(kIOMainPortDefault, IOServiceMatching("IOUSBHostInterface"), &iter)) return 1;
    IOUSBInterfaceInterface300 **usb = NULL;
    io_service_t entry;
    while ((entry = IOIteratorNext(iter))) {
        int match = prop(entry, CFSTR("idVendor")) == 0x2c7c && prop(entry, CFSTR("idProduct")) == 0x0125 &&
                    prop(entry, CFSTR("locationID")) == location && prop(entry, CFSTR("bInterfaceNumber")) == 2 &&
                    prop(entry, CFSTR("bInterfaceClass")) == 255;
        if (match) {
            IOCFPlugInInterface **plugin = NULL;
            SInt32 score;
            IOReturn e = IOCreatePlugInInterfaceForService(entry, kIOUSBInterfaceUserClientTypeID, kIOCFPlugInInterfaceID, &plugin, &score);
            if (e == kIOReturnSuccess && plugin) {
                (*plugin)->QueryInterface(plugin, CFUUIDGetUUIDBytes(kIOUSBInterfaceInterfaceID300), (LPVOID *)&usb);
                (*plugin)->Release(plugin);
            }
        }
        IOObjectRelease(entry);
        if (usb) break;
    }
    IOObjectRelease(iter);
    if (!usb) { fprintf(stderr, "selected EC20 AT interface absent\n"); return 2; }
    IOReturn e = (*usb)->USBInterfaceOpen(usb);
    if (e != kIOReturnSuccess) { fprintf(stderr, "EC20 AT interface unavailable (0x%x); close other AT clients\n", e); (*usb)->Release(usb); return 3; }
    UInt8 count = 0, in = 0, out = 0;
    (*usb)->GetNumEndpoints(usb, &count);
    for (UInt8 pipe = 1; pipe <= count; pipe++) {
        UInt8 direction, number, type, interval;
        UInt16 size;
        if ((*usb)->GetPipeProperties(usb, pipe, &direction, &number, &type, &size, &interval) == kIOReturnSuccess && type == kUSBBulk) {
            if (direction == kUSBIn) in = pipe;
            if (direction == kUSBOut) out = pipe;
        }
    }
    int result = 0;
    if (!in || !out) { fprintf(stderr, "EC20 AT bulk endpoints absent\n"); result = 4; goto done; }
    signal(SIGINT, stop); signal(SIGTERM, stop); signal(SIGPIPE, stop);
    while (running) {
        struct pollfd stdin_poll = { STDIN_FILENO, POLLIN, 0 };
        if (poll(&stdin_poll, 1, 0) > 0) {
            unsigned char data[4096];
            ssize_t n = read(STDIN_FILENO, data, sizeof(data));
            if (n <= 0) break;
            e = (*usb)->WritePipeTO(usb, out, data, (UInt32)n, 1000, 1000);
            if (e != kIOReturnSuccess) { result = 5; break; }
        }
        unsigned char data[4096];
        UInt32 n = sizeof(data);
        e = (*usb)->ReadPipeTO(usb, in, data, &n, 100, 100);
        if (e == kIOReturnSuccess) {
            size_t sent = 0;
            while (sent < n && running) {
                ssize_t written = write(STDOUT_FILENO, data + sent, n - sent);
                if (written <= 0) { running = 0; break; }
                sent += written;
            }
        } else if (running && e != kIOReturnTimeout && e != kIOUSBTransactionTimeout) { result = 6; break; }
    }
done:
    (*usb)->USBInterfaceClose(usb);
    (*usb)->Release(usb);
    if (result) fprintf(stderr, "EC20 USB transport ended (%d)\n", result);
    return result;
}
int main(int argc, char **argv) {
    if (argc == 2 && !strcmp(argv[1], "list")) return devices();
    if (argc == 2 && !strcmp(argv[1], "audio-list")) return audio_devices();
    if (argc == 3 && (!strcmp(argv[1], "at") || !strcmp(argv[1], "audio-info") || !strcmp(argv[1], "audio"))) {
        char *end = NULL;
        unsigned long value = strtoul(argv[2], &end, 16);
        if (strlen(argv[2]) != 8 || !end || *end || value > UINT32_MAX) return 64;
        if (!strcmp(argv[1], "at")) return bridge((uint32_t)value);
        if (!strcmp(argv[1], "audio")) return audio_bridge((uint32_t)value);
        AudioSelection selected = {0};
        if (select_audio((uint32_t)value, &selected)) return 2;
        printf("{\"input\":"); json_string(selected.input); printf(",\"output\":"); json_string(selected.output);
        printf(",\"session\":\"%llu\"}\n", (unsigned long long)selected.session); return 0;
    }
    fprintf(stderr, "usage: ec20-native list | audio-list | at LOCATION | audio-info LOCATION | audio LOCATION\n");
    return 64;
}
