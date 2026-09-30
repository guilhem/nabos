package rabbit

import (
	"context"
	"encoding/binary"
	"errors"
	"github.com/guilhem/nabos/services/internal/hardware"
	"math/rand/v2"
	"os"
	"strings"
	"time"
)

var midi = []string{"choreographies/1noteA4.mp3", "choreographies/1noteB5.mp3", "choreographies/1noteBb4.mp3", "choreographies/1noteC5.mp3", "choreographies/1noteE4.mp3", "choreographies/1noteF4.mp3", "choreographies/1noteF5.mp3", "choreographies/1noteG5.mp3", "choreographies/2notesC6C4.mp3", "choreographies/2notesC6F5.mp3", "choreographies/2notesD4A5.mp3", "choreographies/2notesD4G4.mp3", "choreographies/2notesD5G4.mp3", "choreographies/2notesE5A5.mp3", "choreographies/2notesE5C6.mp3", "choreographies/2notesE5E4.mp3", "choreographies/3notesA4G5G5.mp3", "choreographies/3notesB5A5F5.mp3", "choreographies/3notesB5D5C6.mp3", "choreographies/3notesD4E4G4.mp3", "choreographies/3notesE5A5C6.mp3", "choreographies/3notesE5C6D5.mp3", "choreographies/3notesE5D5A5.mp3", "choreographies/3notesF5C6G5.mp3"}
var palettes = [7][8]RGB{
	{{255, 12, 0}, {0, 255, 31}, {255, 242, 0}, {0, 3, 255}, {255, 242, 0}, {0, 255, 31}, {255, 12, 0}, {0, 0, 0}},
	{{95, 0, 255}, {127, 0, 255}, {146, 0, 255}, {191, 0, 255}, {223, 0, 255}, {255, 0, 223}, {255, 0, 146}, {0, 0, 0}},
	{{255, 255, 255}, {255, 255, 255}, {255, 255, 255}, {255, 255, 255}, {255, 255, 255}, {255, 255, 255}, {255, 255, 255}, {0, 0, 0}},
	{{254, 128, 2}, {243, 68, 2}, {216, 6, 7}, {200, 4, 13}, {170, 0, 24}, {218, 5, 96}, {207, 6, 138}, {0, 0, 0}},
	{{20, 155, 18}, {255, 0, 0}, {252, 243, 5}, {20, 155, 18}, {252, 243, 5}, {255, 0, 0}, {20, 155, 18}, {0, 0, 0}},
	{{252, 238, 71}, {206, 59, 69}, {85, 68, 212}, {78, 167, 82}, {243, 75, 153}, {151, 71, 196}, {255, 255, 255}, {0, 0, 0}},
	{{204, 255, 102}, {204, 255, 0}, {153, 255, 0}, {51, 204, 0}, {0, 153, 51}, {0, 136, 0}, {0, 102, 51}, {0, 0, 0}},
}
var chorLED = [5]uint8{4, 3, 2, 1, 0}

type interpreter struct {
	e             *Engine
	timescale     uint32
	random        uint8
	directions    [2]bool
	palette       [8]RGB
	paletteColors [4]int
}

func waitUntil(ctx context.Context, deadline time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d := time.Until(deadline)
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
func (it *interpreter) binary(ctx context.Context, chor []byte, streaming bool, timescale uint32) error {
	index := 0
	if len(chor) >= 4 && string(chor[:4]) == "\x01\x01\x01\x01" {
		index = 4
	}
	it.timescale = timescale
	next := time.Now()
	pending := map[uint8]RGB{}
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		colors := make([]hardware.Color, 0, len(pending))
		for index := uint8(0); index < 5; index++ {
			if rgb, ok := pending[index]; ok {
				colors = append(colors, color(index, rgb))
			}
		}
		clear(pending)
		return it.e.hw.SetLeds(ctx, colors)
	}
	for index < len(chor) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		delay := time.Duration(chor[index]) * time.Duration(it.timescale) * time.Millisecond
		if delay > 0 {
			if err := flush(); err != nil {
				return err
			}
			next = next.Add(delay)
			if err := waitUntil(ctx, next); err != nil {
				return err
			}
		}
		index++
		if index == len(chor) {
			break
		}
		op := chor[index]
		index++
		// Every opcode validates its entire operand span before accessing the bytes.
		size := 0
		switch op {
		case 0, 16, 19, 255:
		case 1, 10:
			size = 1
		case 7:
			size = 6
		case 8, 9, 18:
			size = 3
		case 14, 17, 20:
			size = 2
		default:
			return flush()
		}
		if index+size > len(chor) {
			return errors.New("truncated choreography")
		}
		args := chor[index : index+size]
		base := index
		index += size
		led := func() (uint8, error) {
			if args[0] > 4 {
				return 0, errors.New("invalid choreography LED")
			}
			return chorLED[args[0]], nil
		}
		if streaming && op != 0 && op != 1 && op != 7 && op != 10 && op != 14 && op != 255 {
			return flush()
		}
		switch op {
		case 0:
		case 1:
			if !streaming {
				it.timescale = 10 * uint32(args[0])
			}
		case 7:
			l, err := led()
			if err != nil {
				return err
			}
			pending[l] = RGB{args[1], args[2], args[3]}
		case 8:
			if args[0] > 1 {
				return errors.New("invalid choreography motor")
			}
			if err := it.e.hw.MoveEar(ctx, args[0], args[1], args[2] != 0); err != nil {
				return err
			}
		case 9:
			for i := uint8(0); i < 5; i++ {
				pending[i] = RGB{args[0], args[1], args[2]}
			}
		case 10:
			l, err := led()
			if err != nil {
				return err
			}
			pending[l] = RGB{}
		case 14:
			l, err := led()
			if err != nil {
				return err
			}
			ix := int(args[1] & 7)
			if streaming {
				ix = it.paletteColors[args[1]&3]
			}
			pending[l] = it.palette[ix]
		case 16:
			if path := it.e.res.find(false, midi[rand.IntN(len(midi))]); path != "" {
				if _, err := it.e.audio.start(ctx, source{"file", path}); err != nil {
					return err
				}
			}
		case 17:
			if args[0] > 1 {
				return errors.New("invalid choreography motor")
			}
			if err := it.e.hw.StepEar(ctx, args[0], args[1], it.directions[args[0]]); err != nil {
				return err
			}
		case 18:
			if it.random != args[0] {
				index = base + int(int16(binary.BigEndian.Uint16(args[1:]))) + 3
				if index < 0 || index > len(chor) {
					return errors.New("invalid choreography branch")
				}
			}
		case 19:
			if err := flush(); err != nil {
				return err
			}
			if err := it.e.hw.WaitEarsIdle(ctx); err != nil {
				return err
			}
			if err := it.e.audio.waitCurrent(ctx); err != nil {
				return err
			}
		case 20:
			if args[0] > 1 {
				return errors.New("invalid choreography motor")
			}
			it.directions[args[0]] = args[1] != 0
		case 255:
			return flush()
		}
	}
	return flush()
}
func (e *Engine) choreography(ctx context.Context, reference string) error {
	it := interpreter{e: e, random: uint8(rand.Uint32N(256) * 30 >> 8)}
	if strings.HasPrefix(reference, streamingURN) {
		palette := -1
		if rest := strings.TrimPrefix(reference, streamingURN); len(rest) == 2 {
			palette = int(rest[1] - '0')
			if palette >= len(palettes) {
				palette = -1
			}
		}
		chance := -1
		for ctx.Err() == nil {
			if chance < 0 {
				chance = 0
				l, r := uint8(0), uint8(10)
				if rand.IntN(2) == 0 {
					l, r = r, l
				}
				if err := e.hw.MoveEar(ctx, 0, l, false); err != nil {
					return err
				}
				if err := e.hw.MoveEar(ctx, 1, r, false); err != nil {
					return err
				}
			} else if rand.IntN(chance+1) == 0 {
				positions := [4]uint8{0, 5, 10, 14}
				for ear := uint8(0); ear < 2; ear++ {
					if err := e.hw.MoveEar(ctx, ear, positions[rand.IntN(4)], false); err != nil {
						return err
					}
				}
				chance = (chance + 1) % 4
			}
			path := e.res.find(true, "system/streaming/*.chor")
			if path == "" {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			tempo := uint32(160 + rand.IntN(91))
			loops := 3 + rand.IntN(18)
			pick := palette
			if pick < 0 {
				pick = rand.IntN(len(palettes))
			}
			it.palette = palettes[pick]
			for i := range it.paletteColors {
				it.paletteColors[i] = rand.IntN(8)
			}
			for range loops {
				if err := it.binary(ctx, data, true, tempo); err != nil {
					return err
				}
			}
		}
		return ctx.Err()
	}
	path := e.res.find(true, reference)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return it.binary(ctx, data, false, 0)
}
