package atmodem

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

var gsmAlphabet = []rune("@£$¥èéùìòÇ\nØø\rÅåΔ_ΦΓΛΩΠΨΣΘΞ\x1bÆæßÉ !\"#¤%&'()*+,-./0123456789:;<=>?¡ABCDEFGHIJKLMNOPQRSTUVWXYZÄÖÑÜ§¿abcdefghijklmnopqrstuvwxyzäöñüà")
var gsmExtensions = map[byte]rune{10: '\f', 20: '^', 40: '{', 41: '}', 47: '\\', 60: '[', 61: '~', 62: ']', 64: '|', 101: '€'}

type Submit struct {
	Hex    string
	Length int
}

func gsmEncode(text string) ([]byte, bool) {
	var out []byte
	for _, r := range text {
		found := false
		for i, value := range gsmAlphabet {
			if r == value && i != 27 {
				out = append(out, byte(i))
				found = true
				break
			}
		}
		if !found {
			for i, value := range gsmExtensions {
				if r == value {
					out = append(out, 27, i)
					found = true
					break
				}
			}
		}
		if !found {
			return nil, false
		}
	}
	return out, true
}
func packSeptets(septets, header []byte) ([]byte, int) {
	padding := (len(header)*8 + 6) / 7
	bits := padding*7 + len(septets)*7
	out := make([]byte, (bits+7)/8)
	copy(out, header)
	for i, s := range septets {
		for bit := 0; bit < 7; bit++ {
			if s&(1<<bit) != 0 {
				at := padding*7 + i*7 + bit
				out[at/8] |= 1 << uint(at%8)
			}
		}
	}
	return out, padding + len(septets)
}

// EncodeSubmit uses GSM-7 where possible, UCS-2 otherwise, and includes an
// 8-bit concatenation header for at most three parts. No SMSC override is used.
func EncodeSubmit(number, text string, reference byte) ([]Submit, error) {
	digits := strings.TrimPrefix(number, "+")
	if len(digits) < 3 || len(digits) > 20 {
		return nil, errors.New("invalid SMS address")
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return nil, errors.New("invalid SMS address")
		}
	}
	address := make([]byte, (len(digits)+1)/2)
	for i := range address {
		lo := digits[i*2] - '0'
		hi := byte(15)
		if i*2+1 < len(digits) {
			hi = digits[i*2+1] - '0'
		}
		address[i] = lo | hi<<4
	}
	data, gsm := gsmEncode(text)
	single, multi := 160, 153
	if !gsm {
		single, multi = 140, 134
		data = nil
		for _, r := range text {
			if r > 0xffff || r >= 0xd800 && r <= 0xdfff {
				return nil, errors.New("SMS requires BMP characters")
			}
			data = append(data, byte(r>>8), byte(r))
		}
	}
	if len(data) == 0 {
		return nil, errors.New("empty SMS")
	}
	var parts [][]byte
	if len(data) <= single {
		parts = append(parts, data)
	} else {
		for len(data) > 0 {
			n := min(multi, len(data))
			if gsm && data[n-1] == 27 {
				n--
			}
			parts = append(parts, append([]byte{}, data[:n]...))
			data = data[n:]
		}
	}
	if len(parts) > 3 {
		return nil, errors.New("SMS exceeds three parts")
	}
	var result []Submit
	for i, part := range parts {
		first, dcs := byte(1), byte(0)
		var header []byte
		if !gsm {
			dcs = 8
		}
		if len(parts) > 1 {
			first |= 0x40
			header = []byte{5, 0, 3, reference, byte(len(parts)), byte(i + 1)}
		}
		ud := append(append([]byte{}, header...), part...)
		length := len(ud)
		if gsm {
			ud, length = packSeptets(part, header)
		}
		toa := byte(0x81)
		if strings.HasPrefix(number, "+") {
			toa = 0x91
		}
		pdu := []byte{0, first, 0, byte(len(digits)), toa}
		pdu = append(pdu, address...)
		pdu = append(pdu, 0, dcs, byte(length))
		pdu = append(pdu, ud...)
		result = append(result, Submit{Hex: strings.ToUpper(hex.EncodeToString(pdu)), Length: len(pdu) - 1})
	}
	return result, nil
}

type Delivery struct {
	Sender, Text, Timestamp string
	Reference, Total, Part  int
}

func unpackSeptets(data []byte, offset, count int) (string, error) {
	if offset+count*7 > len(data)*8 || count < 0 {
		return "", errors.New("truncated GSM data")
	}
	var out []rune
	escape := false
	for i := 0; i < count; i++ {
		var s byte
		for bit := 0; bit < 7; bit++ {
			at := offset + i*7 + bit
			s |= ((data[at/8] >> uint(at%8)) & 1) << uint(bit)
		}
		if escape {
			r, ok := gsmExtensions[s]
			if !ok {
				return "", errors.New("invalid GSM extension")
			}
			out = append(out, r)
			escape = false
		} else if s == 27 {
			escape = true
		} else {
			out = append(out, gsmAlphabet[s])
		}
	}
	if escape {
		return "", errors.New("truncated GSM extension")
	}
	return string(out), nil
}
func swapped(b byte) (int, bool) { a, c := int(b&15), int(b>>4); return a*10 + c, a < 10 && c < 10 }
func DecodeDelivery(text string) (Delivery, error) {
	var d Delivery
	b, err := hex.DecodeString(strings.TrimSpace(text))
	if err != nil || len(b) < 2 {
		return d, errors.New("invalid SMS PDU")
	}
	smsc := int(b[0])
	if smsc+2 > len(b) {
		return d, errors.New("truncated SMSC")
	}
	b = b[1+smsc:]
	first := b[0]
	if first&3 != 0 || len(b) < 3 {
		return d, errors.New("not an SMS delivery")
	}
	length, toa := int(b[1]), b[2]
	n := (length + 1) / 2
	if n > 64 || len(b) < 3+n+10 {
		return d, errors.New("truncated delivery")
	}
	address := b[3 : 3+n]
	if toa&0x70 == 0x50 {
		d.Sender, err = unpackSeptets(address, 0, length*4/7)
		if err != nil {
			return d, err
		}
	} else {
		var digits strings.Builder
		if toa&0x70 == 0x10 {
			digits.WriteByte('+')
		}
		for i := 0; i < length; i++ {
			digit := (address[i/2] >> uint((i%2)*4)) & 15
			if digit > 9 {
				return d, errors.New("invalid sender")
			}
			digits.WriteByte('0' + digit)
		}
		d.Sender = digits.String()
	}
	b = b[3+n:]
	dcs := b[1]
	stamp := b[2:9]
	udl := int(b[9])
	data := b[10:]
	values := make([]int, 6)
	valid := true
	for i := range values {
		var ok bool
		values[i], ok = swapped(stamp[i])
		valid = valid && ok
	}
	tz := stamp[6]
	negative := tz&8 != 0
	tz &^= 8
	quarters, ok := swapped(tz)
	valid = valid && ok && quarters <= 56
	if valid {
		seconds := quarters * 15 * 60
		if negative {
			seconds = -seconds
		}
		t := time.Date(2000+values[0], time.Month(values[1]), values[2], values[3], values[4], values[5], 0, time.FixedZone("SMS", seconds))
		if int(t.Month()) == values[1] && t.Day() == values[2] && values[3] < 24 && values[4] < 60 && values[5] < 60 {
			d.Timestamp = t.Format(time.RFC3339)
		}
	}
	header := 0
	if first&0x40 != 0 {
		if len(data) == 0 {
			return d, errors.New("missing UDH")
		}
		header = int(data[0]) + 1
		if header > len(data) {
			return d, errors.New("truncated UDH")
		}
		for at := 1; at < header; {
			if at+2 > header {
				return d, errors.New("invalid UDH")
			}
			kind, size := data[at], int(data[at+1])
			at += 2
			if at+size > header {
				return d, errors.New("invalid UDH length")
			}
			if kind == 0x24 || kind == 0x25 {
				return d, errors.New("national language shift table unsupported")
			}
			if kind == 0 && size == 3 {
				d.Reference = int(data[at])
				d.Total = int(data[at+1])
				d.Part = int(data[at+2])
			}
			if kind == 8 && size == 4 {
				d.Reference = int(binary.BigEndian.Uint16(data[at:]))
				d.Total = int(data[at+2])
				d.Part = int(data[at+3])
			}
			at += size
		}
		if d.Total != 0 && (d.Part < 1 || d.Part > d.Total) {
			return d, errors.New("invalid multipart sequence")
		}
	}
	// Compressed, binary, and national language tables are not decoded as text.
	if dcs&0xc0 == 0 && dcs&0x20 == 0 && dcs&12 == 0 || dcs&0xf0 == 0xf0 && dcs&4 == 0 || dcs&0xf0 == 0xc0 || dcs&0xf0 == 0xd0 {
		offset := (header*8 + 6) / 7
		d.Text, err = unpackSeptets(data, offset*7, udl-offset)
	} else if dcs == 8 || dcs&0xf0 == 0xe0 {
		if udl > len(data) || udl < header || (udl-header)%2 != 0 {
			return d, errors.New("truncated UCS-2")
		}
		var units []uint16
		for at := header; at < udl; at += 2 {
			units = append(units, binary.BigEndian.Uint16(data[at:]))
		}
		d.Text = string(utf16.Decode(units))
	} else {
		return d, fmt.Errorf("unsupported SMS alphabet %s", strconv.Itoa(int(dcs)))
	}
	return d, err
}
