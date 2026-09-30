package rabbit

import (
	"context"
	"errors"
	"github.com/guilhem/nabos/services/internal/hardware"
	"time"
)

func color(index uint8, rgb RGB) hardware.Color {
	return hardware.Color{Index: index, Red: rgb[0], Green: rgb[1], Blue: rgb[2]}
}
func (e *Engine) all(ctx context.Context, rgb RGB) error {
	v := make([]hardware.Color, 5)
	for i := range v {
		v[i] = color(uint8(i), rgb)
	}
	return e.hw.SetLeds(ctx, v)
}
func (e *Engine) clearInfo(ctx context.Context) error {
	return e.hw.SetLeds(ctx, []hardware.Color{color(1, RGB{}), color(2, RGB{}), color(3, RGB{})})
}
func (e *Engine) move(ctx context.Context, left, right uint8, rgb RGB) error {
	l, r, err := e.hw.ReadEars(ctx, false)
	if err != nil {
		return err
	}
	if l != int16(left) || r != int16(right) {
		if err = e.all(ctx, rgb); err != nil {
			return err
		}
		if err = e.hw.MoveEar(ctx, 0, left, false); err != nil {
			return err
		}
		if err = e.hw.MoveEar(ctx, 1, right, false); err != nil {
			return err
		}
		if err = e.hw.WaitEarsIdle(ctx); err != nil {
			return err
		}
	}
	return e.all(ctx, RGB{})
}

type chorRunner struct {
	e       *Engine
	cancel  context.CancelFunc
	done    chan error
	current string
}

func (r *chorRunner) stop() error {
	if r.cancel == nil {
		return nil
	}
	r.cancel()
	err := <-r.done
	r.cancel = nil
	r.current = ""
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
func (r *chorRunner) start(ctx context.Context, reference string) error {
	if r.current == reference && r.done != nil {
		select {
		case err := <-r.done:
			r.cancel()
			r.cancel = nil
			r.current = ""
			if err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
		default:
			return nil
		}
	}
	if err := r.stop(); err != nil {
		return err
	}
	sub, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	r.done = make(chan error, 1)
	r.current = reference
	go func() { r.done <- r.e.choreography(sub, reference) }()
	return nil
}
func (r *chorRunner) wait(ctx context.Context) error {
	if r.cancel == nil {
		return nil
	}
	select {
	case err := <-r.done:
		r.cancel()
		r.cancel = nil
		r.current = ""
		return err
	case <-ctx.Done():
		err := r.stop()
		if err != nil {
			return err
		}
		return ctx.Err()
	}
}
func (r *chorRunner) playAudio(ctx context.Context, files []source) error {
	sub, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- r.e.audio.list(sub, files) }()
	if r.cancel == nil {
		return <-done
	}
	select {
	case err := <-done:
		return err
	case err := <-r.done:
		r.cancel()
		r.cancel = nil
		r.current = ""
		if err != nil {
			cancel()
			clean := <-done
			return errors.Join(err, clean)
		}
		return <-done
	}
}

type loadedItem struct {
	audio []source
	chor  string
}

func (e *Engine) preload(items []Item) []loadedItem {
	loaded := make([]loadedItem, 0, len(items))
	for _, it := range items {
		l := loadedItem{chor: it.Choreography}
		if it.Audio != nil {
			l.audio = make([]source, 0, len(it.Audio))
			for _, spec := range it.Audio {
				if path := e.res.find(false, spec); path != "" {
					l.audio = append(l.audio, source{"file", path})
				}
			}
		}
		if it.Stream != "" {
			l.audio = []source{{"stream", it.Stream}}
		}
		loaded = append(loaded, l)
	}
	return loaded
}
func (e *Engine) loaded(ctx context.Context, runner *chorRunner, items []loadedItem, defaultChor string) error {
	for _, it := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		chor := it.chor
		if chor == "" {
			chor = defaultChor
		}
		if chor != "" {
			if err := runner.start(ctx, chor); err != nil {
				return err
			}
		} else if err := runner.stop(); err != nil {
			return err
		}
		if it.audio != nil {
			if err := runner.playAudio(ctx, it.audio); err != nil {
				return err
			}
			if chor != "" {
				if err := runner.stop(); err != nil {
					return err
				}
			}
		} else if it.chor != "" {
			if err := runner.wait(ctx); err != nil {
				return err
			}
		}
	}
	return nil
}
func (e *Engine) sequence(ctx context.Context, c Command) error {
	runner := chorRunner{e: e}
	var result error
	if c.Action == Message {
		result = e.move(ctx, 0, 0, RGB{255, 0, 0})
		signature := Item{}
		if c.Signature != nil {
			signature = *c.Signature
		}
		sig := e.preload([]Item{signature})
		body := e.preload(c.Body)
		if result == nil {
			for _, part := range [][]loadedItem{sig, body, sig} {
				if result = e.loaded(ctx, &runner, part, streamingURN); result != nil {
					break
				}
			}
		}
	} else {
		result = e.loaded(ctx, &runner, e.preload(c.Sequence), "")
	}
	return errors.Join(result, runner.stop(), e.audio.stop())
}
func (e *Engine) animation(ctx context.Context, a Animation, duration time.Duration) error {
	next, start := time.Now(), time.Now()
	step := time.Duration(a.Tempo) * 10 * time.Millisecond
	for i := 0; duration == 0 || time.Since(start) < duration; i++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		f := a.Frames[i%len(a.Frames)]
		if err := e.hw.SetLeds(ctx, []hardware.Color{color(1, f[0]), color(2, f[1]), color(3, f[2])}); err != nil {
			return err
		}
		next = next.Add(step)
		deadline := next
		if duration > 0 && deadline.After(start.Add(duration)) {
			deadline = start.Add(duration)
		}
		if err := waitUntil(ctx, deadline); err != nil {
			return err
		}
	}
	return e.clearInfo(ctx)
}
func (e *Engine) feedback(ctx context.Context, rfid bool) error {
	if rfid {
		return e.sequence(ctx, Command{Action: Play, Sequence: []Item{{Audio: []string{"rfid/rfid.wav"}, Choreography: "system/rfid.chor"}}})
	}
	if p := e.res.find(false, "system/abort.wav"); p != "" {
		return e.audio.list(ctx, []source{{"file", p}})
	}
	return nil
}
func (e *Engine) test(ctx context.Context, kind string) error {
	if kind == "leds" {
		for _, rgb := range []RGB{{}, {255, 0, 0}, {0, 255, 0}, {0, 0, 255}, {255, 255, 255}, {127, 127, 127}, {}} {
			for i := uint8(0); i < 5; i++ {
				if err := e.hw.SetLeds(ctx, []hardware.Color{color(i, rgb)}); err != nil {
					return err
				}
				if err := waitUntil(ctx, time.Now().Add(200*time.Millisecond)); err != nil {
					return err
				}
			}
			if err := waitUntil(ctx, time.Now().Add(time.Second)); err != nil {
				return err
			}
		}
		s, ok := e.hw.Snapshot()
		if !ok || !s.Leds {
			return errors.New("test_failed")
		}
		return nil
	}
	l, r, err := e.hw.ReadEars(ctx, false)
	if err != nil {
		return err
	}
	for i := uint8(0); i < 2; i++ {
		if err = e.hw.MoveEar(ctx, i, 8, true); err != nil {
			return err
		}
	}
	if err = e.hw.WaitEarsIdle(ctx); err != nil {
		return err
	}
	for _, backward := range []bool{false, true} {
		for range 17 {
			if err = e.hw.StepEar(ctx, 0, 1, backward); err != nil {
				return err
			}
			if err = e.hw.StepEar(ctx, 1, 1, !backward); err != nil {
				return err
			}
			if err = e.hw.WaitEarsIdle(ctx); err != nil {
				return err
			}
			if err = waitUntil(ctx, time.Now().Add(200*time.Millisecond)); err != nil {
				return err
			}
		}
	}
	if err = e.move(ctx, 0, 0, RGB{}); err != nil {
		return err
	}
	if l >= 0 {
		if err = e.hw.MoveEar(ctx, 0, uint8(l), false); err != nil {
			return err
		}
	}
	if r >= 0 {
		if err = e.hw.MoveEar(ctx, 1, uint8(r), false); err != nil {
			return err
		}
	}
	if err = e.hw.WaitEarsIdle(ctx); err != nil {
		return err
	}
	status, online := e.hw.Snapshot()
	if !online || status.LeftEar != "ok" || status.RightEar != "ok" {
		return errors.New("test_failed")
	}
	return nil
}
func (e *Engine) write(ctx context.Context, t TagWrite) (Result, error) {
	if s, ok := e.hw.Snapshot(); !ok || s.RFID == "none" || s.RFID == "" {
		return Result{}, errors.New("no_reader")
	}
	if err := e.hw.SetLeds(ctx, []hardware.Color{color(0, RGB{255, 0, 0})}); err != nil {
		return Result{}, err
	}
	// Preserve the admitted ID even when the caller disappears during StartWrite.
	admission, cancel := context.WithTimeout(context.WithoutCancel(ctx), hardware.Timeout)
	id, err := e.hw.StartWrite(admission, t.Tech, t.UID, t.Picture, t.App, t.Data, t.Timeout)
	cancel()
	if err != nil {
		return Result{}, err
	}
	outcome, err := e.hw.WaitWrite(ctx, id)
	if err != nil || ctx.Err() != nil {
		clean, cancel := context.WithTimeout(context.WithoutCancel(ctx), 65*time.Second)
		defer cancel()
		stop := e.hw.CancelWrite(clean, id)
		_, wait := e.hw.WaitWrite(clean, id)
		if stop != nil || wait != nil {
			e.hw.DropContext(ctx)
			return Result{}, errors.Join(err, stop, wait)
		}
		if ctx.Err() != nil {
			return Result{}, ctx.Err()
		}
		return Result{}, err
	}
	switch outcome {
	case "completed":
		return Result{Status: "ok", UID: append([]byte(nil), t.UID...)}, nil
	case "canceled":
		return Result{}, context.Canceled
	case "timeout":
		return Result{}, errors.New("timeout")
	case "no-reader":
		return Result{}, errors.New("no_reader")
	default:
		return Result{}, errors.New("write_failed")
	}
}
