package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/godbus/dbus/v5"
	"regexp"
	"strings"
)

var smsPathPattern = regexp.MustCompile(`^/org/freedesktop/ModemManager1/SMS/[0-9]{1,12}$`)

func (b *ModemBackend) PrepareSMS(ctx context.Context, number, text string) (SMSRef, error) {
	b.mu.Lock()
	all, err := b.get(ctx)
	if err != nil {
		b.mu.Unlock()
		return SMSRef{}, err
	}
	path, r, _, _, err := parseObjects(all, b.expected)
	conn := b.conn
	if err != nil || path == "" || !r.Present || !r.SIMReady || !r.Messaging {
		b.mu.Unlock()
		return SMSRef{}, errors.New("SMS service not ready")
	}
	var owner string
	err = conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", dbus.FlagNoAutoStart, destination).Store(&owner)
	b.mu.Unlock()
	if err != nil {
		return SMSRef{}, err
	}
	var sms dbus.ObjectPath
	err = conn.Object(owner, path).CallWithContext(ctx, modemIF+".Messaging.Create", dbus.FlagNoAutoStart, map[string]dbus.Variant{"number": dbus.MakeVariant(number), "text": dbus.MakeVariant(text)}).Store(&sms)
	if err != nil {
		return SMSRef{}, err
	}
	return SMSRef{Owner: owner, Path: string(sms), DeviceID: r.ID}, nil
}
func (b *ModemBackend) smsObject(ref SMSRef) (dbus.BusObject, error) {
	if !strings.HasPrefix(ref.Owner, ":") || !smsPathPattern.MatchString(ref.Path) || ref.DeviceID == "" || (b.expected != "" && ref.DeviceID != b.expected) {
		return nil, errors.New("invalid or mismatched SMS handle")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.connect(); err != nil {
		return nil, err
	}
	return b.conn.Object(ref.Owner, dbus.ObjectPath(ref.Path)), nil
}
func (b *ModemBackend) SubmitSMS(ctx context.Context, ref SMSRef) error {
	obj, err := b.smsObject(ref)
	if err != nil {
		return err
	}
	return obj.CallWithContext(ctx, smsIF+".Send", dbus.FlagNoAutoStart).Err
}
func (b *ModemBackend) ReadSMSState(ctx context.Context, ref SMSRef) (uint32, error) {
	obj, err := b.smsObject(ref)
	if err != nil {
		return 0, err
	}
	var props map[string]dbus.Variant
	if err = obj.CallWithContext(ctx, "org.freedesktop.DBus.Properties.GetAll", dbus.FlagNoAutoStart, smsIF).Store(&props); err != nil {
		return 0, err
	}
	state, ok := props["State"].Value().(uint32)
	if !ok {
		return 0, errors.New("missing SMS state")
	}
	return state, nil
}
func (b *DemoBackend) PrepareSMS(ctx context.Context, number, text string) (SMSRef, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.serial++
	return SMSRef{Owner: "demo", Path: fmt.Sprint(b.serial), DeviceID: "demo-ec20"}, nil
}
func (b *DemoBackend) SubmitSMS(ctx context.Context, ref SMSRef) error { return nil }
func (b *DemoBackend) ReadSMSState(ctx context.Context, ref SMSRef) (uint32, error) {
	return smsStateSent, nil
}
