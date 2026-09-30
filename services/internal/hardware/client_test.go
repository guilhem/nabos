package hardware

import (
	"context"
	"errors"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestWireTypesAndDriverBoundary(t *testing.T) {
	if got := dbus.SignatureOf(Status{}).String(); got != "(sbssbbsnn)" {
		t.Fatal("status wire", got)
	}
	if got := dbus.SignatureOf([]Color{}).String(); got != "a(yyyy)" {
		t.Fatal("LED wire", got)
	}
	tag := Tag{}
	if got := dbus.SignatureOf(tag.Removed, tag.Tech, tag.UID, tag.Support, tag.Locked, tag.Formatted, tag.Picture, tag.App, tag.Data).String(); got != "bsaysbbyyay" {
		t.Fatal("tag wire", got)
	}
	client := New(nil)
	ctx := context.Background()
	for _, colors := range [][]Color{{{Index: 5}}, {{Index: 1}, {Index: 1}}, make([]Color, 6)} {
		if err := client.SetLeds(ctx, colors); err == nil || errors.Is(err, ErrUnavailable) {
			t.Fatal("invalid LEDs reached transport", colors, err)
		}
	}
	if err := client.MoveEar(ctx, 2, 255, false); err == nil || errors.Is(err, ErrUnavailable) {
		t.Fatal("invalid motor reached transport", err)
	}
	if err := client.StepEar(ctx, 2, 255, true); err == nil || errors.Is(err, ErrUnavailable) {
		t.Fatal("invalid motor reached transport", err)
	}
	for _, length := range []int{4, 7, 10} {
		if !ValidTag("iso14443a_t2t", make([]byte, length), make([]byte, 32), 60) {
			t.Fatal("valid tag rejected", length)
		}
	}
	for _, length := range []int{0, 3, 5, 6, 8, 9, 11} {
		if ValidTag("iso14443a_t2t", make([]byte, length), nil, 20) {
			t.Fatal("unsupported UID length", length)
		}
	}
	for _, tc := range []struct {
		tech         string
		length, data int
		timeout      uint32
	}{{"other", 8, 0, 20}, {"st25tb", 7, 0, 20}, {"st25tb", 8, 33, 20}, {"st25tb", 8, 0, 0}, {"st25tb", 8, 0, 61}} {
		if ValidTag(tc.tech, make([]byte, tc.length), make([]byte, tc.data), tc.timeout) {
			t.Fatal(tc)
		}
	}
}
