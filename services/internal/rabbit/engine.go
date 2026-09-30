// Package rabbit owns gameplay. The event loop owns state; its activity worker
// drains physical cleanup before a subsequent worker can start.
package rabbit

import (
	"context"
	"errors"
	"github.com/guilhem/nabos/services/internal/hardware"
	"log"
	"sync"
	"sync/atomic"
	"time"
)

type request struct {
	ctx      context.Context
	command  Command
	result   chan Result
	accepted chan error
	started  chan<- struct{}
}
type maintenance struct {
	blocked bool
	reply   chan error
}
type settings struct{ locale, network string }
type setupDone struct{ version uint64 }
type taskResult struct {
	kind         string
	result       Result
	err, cleanup error
	left, right  int16
}
type activity struct {
	kind     string
	cancel   context.CancelFunc
	feedback atomic.Bool
	request  *request
}
type info struct {
	id        string
	animation Animation
}
type cancelRequest struct{ request *request }
type Engine struct {
	options                     Options
	handlers                    Handlers
	hw                          *hardware.Client
	audio                       audio
	res                         resources
	mu                          sync.RWMutex
	snapshot                    State
	initialized, ready, started bool
	inputs                      chan any
	done                        chan struct{}
	// Fields below belong exclusively to loop.
	state                     string
	blocked, failed, stopping bool
	queue                     []*request
	task                      *activity
	infos                     []info
	indicator                 *Animation
	network                   string
	left, right               uint8
	refresh, transition, rfid bool
	detectAt                  time.Time
	controls                  []maintenance
	onlinePending             bool
	transitionVersion         uint64
	cancelWaiters             []*request
}

func New(o Options, h Handlers) *Engine {
	e := &Engine{options: o, handlers: h, inputs: make(chan any, 256), done: make(chan struct{}), state: "idle", blocked: true, network: "ok", refresh: true, transition: true, res: resources{sounds: append([]string(nil), o.SoundsDirs...), chors: append([]string(nil), o.ChorDirs...), locale: "fr_FR"}}
	e.hw = hardware.New(func(ev hardware.Event) {
		select {
		case e.inputs <- ev:
		case <-e.done:
		}
	})
	e.snapshot = State{State: "idle", Version: o.Version, Hardware: hardware.Status{Left: -1, Right: -1}}
	return e
}
func (e *Engine) Start(ctx context.Context) error {
	e.mu.Lock()
	if e.started {
		e.mu.Unlock()
		return errors.New("rabbit already started")
	}
	if ctx.Err() != nil {
		e.mu.Unlock()
		return ctx.Err()
	}
	e.started = true
	e.mu.Unlock()
	// The hardware connection survives until application activity has drained.
	if err := e.hw.Start(context.Background()); err != nil {
		return err
	}
	go e.loop(ctx)
	return nil
}
func cloneState(s State) State {
	if s.Playing != nil {
		id := *s.Playing
		s.Playing = &id
	}
	return s
}
func (e *Engine) State() (State, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return cloneState(e.snapshot), e.initialized
}

// EventCurrent fences application queues against events from a departed hardware owner.
func (e *Engine) EventCurrent(ev Event) bool {
	return e.hw.Current(hardware.Event{Generation: ev.generation})
}
func (e *Engine) Ready() bool { e.mu.RLock(); defer e.mu.RUnlock(); return e.ready }
func (e *Engine) publish() {
	status, online := e.hw.Snapshot()
	s := State{State: e.state, Ears: EarPositions{int(e.left), int(e.right)}, Hardware: status, Version: e.options.Version}
	if e.task != nil && e.task.request != nil {
		id := e.task.request.command.ID
		s.Playing = &id
	}
	e.mu.Lock()
	e.snapshot = s
	e.initialized = true
	e.ready = online && status.Ready() && !e.blocked && !e.failed && !e.stopping
	e.mu.Unlock()
	if e.handlers.OnState != nil {
		e.handlers.OnState(cloneState(s))
	}
}
func cloneCommand(c Command) Command {
	cloneItems := func(items []Item) []Item {
		if items == nil {
			return nil
		}
		out := append([]Item{}, items...)
		for i := range out {
			if out[i].Audio != nil {
				out[i].Audio = append([]string{}, out[i].Audio...)
			}
		}
		return out
	}
	c.Sequence = cloneItems(c.Sequence)
	c.Body = cloneItems(c.Body)
	if c.Signature != nil {
		it := cloneItems([]Item{*c.Signature})[0]
		c.Signature = &it
	}
	if c.Animation != nil {
		a := *c.Animation
		a.Frames = append([][3]RGB(nil), a.Frames...)
		c.Animation = &a
	}
	if c.Tag != nil {
		tag := *c.Tag
		tag.UID = append([]byte(nil), tag.UID...)
		tag.Data = append([]byte(nil), tag.Data...)
		c.Tag = &tag
	}
	if c.Left != nil {
		v := *c.Left
		c.Left = &v
	}
	if c.Right != nil {
		v := *c.Right
		c.Right = &v
	}
	return c
}
func (e *Engine) submit(ctx context.Context, c Command, started chan<- struct{}, detached bool) (*request, error) {
	if err := validate(c); err != nil {
		return nil, err
	}
	if c.ID == "" {
		c.ID = NewID()
	}
	c = cloneCommand(c)
	lifetime := ctx
	if detached {
		lifetime = context.WithoutCancel(ctx)
	}
	r := &request{ctx: lifetime, command: c, result: make(chan Result, 1), accepted: make(chan error, 1), started: started}
	e.mu.RLock()
	running := e.started
	e.mu.RUnlock()
	if !running {
		return nil, errors.New("rabbit not started")
	}
	select {
	case e.inputs <- r:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-e.done:
		return nil, errors.New("rabbit stopped")
	}
	// Once enqueued, obtain the admission decision rather than abandoning a handle.
	select {
	case err := <-r.accepted:
		return r, err
	case <-e.done:
		return nil, errors.New("rabbit stopped")
	}
}
func (e *Engine) Send(ctx context.Context, c Command) (string, error) {
	r, err := e.submit(ctx, c, nil, true)
	if r == nil {
		return "", err
	}
	return r.command.ID, err
}
func (e *Engine) Do(ctx context.Context, c Command) (Result, error) { return e.DoStarted(ctx, c, nil) }
func (e *Engine) DoStarted(ctx context.Context, c Command, started chan<- struct{}) (Result, error) {
	r, err := e.submit(ctx, c, started, false)
	if err != nil {
		return Result{}, err
	}
	select {
	case result := <-r.result:
		return result, nil
	case <-ctx.Done():
		// Keep the caller's activity gate until cancellation and physical cleanup finish.
		select {
		case e.inputs <- cancelRequest{r}:
		case <-e.done:
			return Result{}, ctx.Err()
		}
		clean, cancel := context.WithTimeout(context.Background(), 75*time.Second)
		defer cancel()
		select {
		case result := <-r.result:
			if result.Error != "" {
				return result, result.Err()
			}
			return result, ctx.Err()
		case <-clean.Done():
			e.hw.Drop()
			return Result{}, errors.New("rabbit cleanup timed out")
		case <-e.done:
			return Result{}, ctx.Err()
		}
	case <-e.done:
		return Result{}, errors.New("rabbit stopped")
	}
}
func (e *Engine) SetSettings(locale, network string) {
	e.res.setLocale(locale)
	select {
	case e.inputs <- settings{locale, network}:
	case <-e.done:
	}
}
func (e *Engine) SetMaintenance(ctx context.Context, blocked bool) error {
	e.mu.RLock()
	started := e.started
	e.mu.RUnlock()
	if !started && blocked {
		return nil
	}
	if !started {
		return errors.New("rabbit not started")
	}
	m := maintenance{blocked: blocked, reply: make(chan error, 1)}
	select {
	case e.inputs <- m:
	case <-ctx.Done():
		return ctx.Err()
	case <-e.done:
		return errors.New("rabbit stopped")
	}
	select {
	case err := <-m.reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-e.done:
		return errors.New("rabbit stopped")
	}
}
func (e *Engine) Stop(ctx context.Context) {
	e.mu.Lock()
	started := e.started
	e.ready = false
	e.mu.Unlock()
	if !started {
		return
	}
	select {
	case e.inputs <- "stop":
	case <-e.done:
		return
	case <-ctx.Done():
		e.hw.Drop()
		return
	}
	select {
	case <-e.done:
	case <-ctx.Done():
		e.hw.Drop()
		e.mu.Lock()
		e.ready = false
		e.mu.Unlock()
	}
}
func expired(c Command) bool { return !c.Deadline.IsZero() && time.Now().After(c.Deadline) }
func finish(r *request, result Result) {
	if r != nil {
		r.result <- result
	}
}
func failure(err error) Result {
	if err == nil {
		return Result{Status: "ok"}
	}
	if errors.Is(err, context.Canceled) {
		return Result{Status: "canceled"}
	}
	return Result{Status: "error", Error: err.Error()}
}
func (e *Engine) loop(ctx context.Context) {
	defer close(e.done)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	e.publish()
	ctxDone := ctx.Done()
	for {
		e.advance()
		if e.stopping && e.task == nil {
			clean, cancel := cleanupContext()
			err := errors.Join(e.audio.close(), e.hw.Stop(clean))
			cancel()
			if err != nil {
				log.Printf("rabbit shutdown cleanup: %v", err)
			}
			e.mu.Lock()
			e.ready = false
			e.initialized = false
			e.mu.Unlock()
			return
		}
		select {
		case <-ctxDone:
			ctxDone = nil
			e.shutdown()
		case <-tick.C:
			e.sweep()
		case input := <-e.inputs:
			switch v := input.(type) {
			case *request:
				e.command(v)
			case cancelRequest:
				e.cancelOwned(v.request)
			case settings:
				if v.network == "ok" || v.network == "lan" || v.network == "offline" {
					if e.network != v.network {
						e.network = v.network
						e.refresh = true
						e.cancelBackground()
					}
				}
			case maintenance:
				e.maintenance(v)
			case hardware.Event:
				e.hardware(v)
			case setupDone:
				if v.version == e.transitionVersion {
					e.transition = false
				}
			case taskResult:
				e.completed(v)
			case string:
				if v == "stop" {
					e.shutdown()
				}
			}
		}
	}
}
func (e *Engine) shutdown() {
	if e.stopping {
		return
	}
	e.stopping = true
	e.blocked = true
	if e.task != nil {
		e.task.cancel()
	}
	for _, r := range e.queue {
		finish(r, Result{Status: "canceled"})
	}
	e.queue = nil
	for _, m := range e.controls {
		m.reply <- errors.New("rabbit stopped")
	}
	e.controls = nil
	e.publish()
}
func (e *Engine) cancelBackground() {
	if e.task != nil && e.task.request == nil {
		e.task.cancel()
	}
}
func (e *Engine) sweep() {
	kept := e.queue[:0]
	for _, r := range e.queue {
		switch {
		case r.ctx.Err() != nil:
			finish(r, Result{Status: "canceled"})
		case expired(r.command):
			finish(r, Result{Status: "expired"})
		default:
			kept = append(kept, r)
		}
	}
	e.queue = kept
}
func (e *Engine) command(r *request) {
	c := r.command
	reject := func(err error) { r.accepted <- err; finish(r, failure(err)) }
	if e.stopping {
		reject(errors.New("rabbit stopped"))
		return
	}
	if r.ctx.Err() != nil {
		reject(r.ctx.Err())
		return
	}
	if expired(c) {
		r.accepted <- nil
		finish(r, Result{Status: "expired"})
		return
	}
	if c.Action != Gestalt && c.Action != Cancel && (e.blocked || e.failed) {
		reject(errors.New("maintenance or hardware recovery"))
		return
	}
	if c.Action != Gestalt && c.Action != Cancel {
		if _, online := e.hw.Snapshot(); !online {
			reject(hardware.ErrUnavailable)
			return
		}
	}
	r.accepted <- nil
	switch c.Action {
	case Play, Message, Sleep, RfidWrite, Test:
		if c.Action == Sleep && e.state == "asleep" {
			finish(r, Result{Status: "ok"})
			return
		}
		e.queue = append(e.queue, r)
		e.cancelBackground()
	case Cancel:
		if e.task != nil && e.task.request != nil && (c.Target == "" || c.Target == e.task.request.command.ID) {
			owned := e.task.request.command
			if c.Target == "" && !(owned.Cancelable && (owned.Action == Play || owned.Action == Message)) {
				finish(r, Result{Status: "error", Error: "not_cancelable"})
				return
			}
			e.task.cancel()
			e.cancelWaiters = append(e.cancelWaiters, r)
		} else {
			found := false
			for i, q := range e.queue {
				if c.Target != "" && q.command.ID == c.Target {
					e.queue = append(e.queue[:i], e.queue[i+1:]...)
					finish(q, Result{Status: "canceled"})
					found = true
					break
				}
			}
			if found {
				finish(r, Result{Status: "ok"})
			} else {
				finish(r, Result{Status: "error", Error: "not_playing"})
			}
		}
	case Info:
		index := -1
		for i, v := range e.infos {
			if v.id == c.InfoID {
				index = i
				break
			}
		}
		if c.Animation == nil {
			if index >= 0 {
				e.infos = append(e.infos[:index], e.infos[index+1:]...)
			}
		} else if index >= 0 {
			e.infos[index].animation = *c.Animation
		} else if len(e.infos) >= 16 {
			finish(r, Result{Status: "rejected", Error: "too many infos"})
			return
		} else {
			e.infos = append(e.infos, info{c.InfoID, *c.Animation})
		}
		e.refresh = true
		e.cancelBackground()
		finish(r, Result{Status: "ok"})
	case Indicator:
		e.indicator = c.Animation
		e.refresh = true
		e.cancelBackground()
		finish(r, Result{Status: "ok"})
	case Ears:
		if c.Left != nil {
			e.left = *c.Left
		}
		if c.Right != nil {
			e.right = *c.Right
		}
		e.transition = true
		e.transitionVersion++
		e.refresh = true
		e.cancelBackground()
		e.publish()
		finish(r, Result{Status: "ok"})
	case Wakeup:
		if e.state == "asleep" {
			e.state = "idle"
			e.transition = true
			e.transitionVersion++
			e.refresh = true
			e.cancelBackground()
			e.publish()
		}
		finish(r, Result{Status: "ok"})
	case Gestalt:
		finish(r, Result{Status: "ok"})
	}
}
func (e *Engine) cancelOwned(r *request) {
	if e.task != nil && e.task.request == r {
		e.task.cancel()
		return
	}
	for i, q := range e.queue {
		if q == r {
			e.queue = append(e.queue[:i], e.queue[i+1:]...)
			finish(q, Result{Status: "canceled"})
			return
		}
	}
}
func (e *Engine) maintenance(m maintenance) {
	if e.stopping {
		m.reply <- errors.New("rabbit stopped")
		return
	}
	if !m.blocked {
		s, online := e.hw.Snapshot()
		if !online || !s.Ready() || e.failed {
			m.reply <- hardware.ErrUnavailable
			return
		}
		e.blocked = false
		e.refresh = true
		e.transition = true
		e.transitionVersion++
		e.publish()
		m.reply <- nil
		return
	}
	e.blocked = true
	for _, r := range e.queue {
		finish(r, Result{Status: "canceled"})
	}
	e.queue = nil
	e.refresh = true
	e.detectAt = time.Time{}
	e.controls = append(e.controls, m)
	if e.task != nil {
		e.task.cancel()
	}
	e.publish()
}
func (e *Engine) hardware(ev hardware.Event) {
	if !e.hw.SameGeneration(ev) || (ev.Kind != "offline" && !e.hw.Current(ev)) {
		return
	}
	switch ev.Kind {
	case "online":
		for _, r := range e.queue {
			finish(r, failure(hardware.ErrStale))
		}
		e.queue = nil
		e.onlinePending = true
		e.failed = false
		e.refresh = true
		e.transition = true
		e.transitionVersion++
		if e.task != nil {
			e.task.cancel()
		}
		e.publish()
	case "offline":
		e.failed = true
		e.refresh = true
		if e.task != nil {
			e.task.cancel()
		}
		for _, r := range e.queue {
			finish(r, failure(hardware.ErrUnavailable))
		}
		e.queue = nil
		e.publish()
	case "changed":
		e.publish()
	case "button":
		if ev.Button == "click" && e.task != nil && e.task.request != nil {
			c := e.task.request.command
			if (c.Action == Play || c.Action == Message) && c.Cancelable {
				e.task.feedback.Store(true)
				e.task.cancel()
				return
			}
		}
		if e.handlers.OnEvent != nil {
			edge := uint64(0)
			if ev.Button == "down" {
				edge = ev.EdgeMonotonicNS
			}
			e.handlers.OnEvent(Event{generation: ev.Generation, Kind: "button", Button: ev.Button, EdgeMonotonicNS: edge})
		}
	case "ear_moved":
		if e.handlers.OnEvent != nil {
			e.handlers.OnEvent(Event{generation: ev.Generation, Kind: "ear_moved", Ear: ev.Ear})
		}
		if !e.blocked {
			e.detectAt = time.Now().Add(500 * time.Millisecond)
			e.cancelBackground()
		}
	case "rfid":
		if e.handlers.OnEvent != nil {
			e.handlers.OnEvent(Event{generation: ev.Generation, Kind: "rfid", Tag: ev.Tag})
		}
		if ev.Tag != nil && !ev.Tag.Removed && e.state == "idle" && (e.task == nil || e.task.request == nil) && !e.blocked {
			e.rfid = true
			e.refresh = true
			e.cancelBackground()
		}
	}
}
func (e *Engine) launch(kind string, r *request, work func(context.Context) (Result, int16, int16, error)) {
	ctx, cancel := context.WithCancel(e.hw.Bind(context.Background()))
	task := &activity{kind: kind, cancel: cancel, request: r}
	e.task = task
	if r != nil {
		if e.state != "asleep" {
			e.state = "playing"
		}
		e.publish()
	}
	go func() {
		if r != nil && r.started != nil {
			select {
			case r.started <- struct{}{}:
			case <-ctx.Done():
			case <-r.ctx.Done():
				cancel()
			}
		}
		result, left, right, err := work(ctx)
		if task.feedback.Load() {
			fbCtx, fbCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			fb := e.feedback(fbCtx, false)
			fbCancel()
			if fb != nil {
				err = fb
			}
		}
		clean, cleanCancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		cleanup := errors.Join(e.audio.stop(), e.hw.WaitEarsIdle(clean), e.all(clean, RGB{}))
		cleanCancel()
		if cleanup != nil {
			e.hw.DropContext(ctx)
		}
		select {
		case e.inputs <- taskResult{kind: kind, result: result, err: err, cleanup: cleanup, left: left, right: right}:
		case <-e.done:
		}
	}()
}
func (e *Engine) advance() {
	if e.task != nil {
		return
	}
	if e.onlinePending {
		e.audio.resetLoss()
		_, online := e.hw.Snapshot()
		if online {
			e.failed = false
		}
		e.onlinePending = false
		if e.handlers.OnOnline != nil {
			e.handlers.OnOnline()
		}
	}
	if e.stopping {
		return
	}
	if len(e.controls) > 0 {
		e.launch("maintenance", nil, func(ctx context.Context) (Result, int16, int16, error) { return Result{}, 0, 0, e.hw.WaitEarsIdle(ctx) })
		return
	}
	if e.blocked || e.failed {
		return
	}
	s, online := e.hw.Snapshot()
	if !online || !s.Ready() {
		return
	}
	e.sweep()
	if !e.detectAt.IsZero() {
		if time.Now().Before(e.detectAt) {
			return
		}
		e.detectAt = time.Time{}
		e.launch("detect", nil, func(ctx context.Context) (Result, int16, int16, error) {
			l, r, err := e.hw.ReadEars(ctx, true)
			return Result{}, l, r, err
		})
		return
	}
	index := -1
	if e.state == "asleep" {
		for i, r := range e.queue {
			if r.command.Action == RfidWrite || r.command.Action == Test {
				index = i
				break
			}
		}
	} else if len(e.queue) > 0 {
		index = 0
		for i, r := range e.queue {
			if r.command.Action != Sleep {
				index = i
				break
			}
		}
	}
	if index >= 0 {
		r := e.queue[index]
		e.queue = append(e.queue[:index], e.queue[index+1:]...)
		if r.command.Action == Sleep {
			e.state = "asleep"
			e.transition = true
			e.transitionVersion++
			e.refresh = true
			finish(r, Result{Status: "ok"})
			for _, q := range e.queue {
				finish(q, Result{Status: "ok"})
			}
			e.queue = nil
			e.publish()
		} else {
			c := r.command
			e.launch("job", r, func(ctx context.Context) (Result, int16, int16, error) {
				switch c.Action {
				case Play, Message:
					return Result{}, 0, 0, e.sequence(ctx, c)
				case Test:
					return Result{}, 0, 0, e.test(ctx, c.Test)
				case RfidWrite:
					result, err := e.write(ctx, *c.Tag)
					return result, 0, 0, err
				}
				return Result{}, 0, 0, nil
			})
			return
		}
	}
	if e.state == "playing" {
		e.state = "idle"
		e.refresh = true
		e.transition = true
		e.transitionVersion++
		e.publish()
	}
	if !e.refresh {
		return
	}
	e.refresh = false
	asleep, transition, rfid, left, right, network := e.state == "asleep", e.transition, e.rfid, e.left, e.right, e.network
	version := e.transitionVersion
	e.rfid = false
	infos := append([]info(nil), e.infos...)
	indicator := e.indicator
	e.launch("background", nil, func(ctx context.Context) (Result, int16, int16, error) {
		run := func() error {
			if rfid {
				if err := e.feedback(ctx, true); err != nil {
					return err
				}
			}
			if transition {
				if asleep {
					if err := e.all(ctx, RGB{}); err != nil {
						return err
					}
					if err := e.move(ctx, 10, 10, RGB{}); err != nil {
						return err
					}
				} else if err := e.move(ctx, left, right, RGB{255, 0, 255}); err != nil {
					return err
				}
				select {
				case e.inputs <- setupDone{version}:
				case <-e.done:
					return context.Canceled
				}
			}
			if !asleep {
				belly := RGB{255, 0, 255}
				if network == "offline" {
					belly = RGB{255, 0, 0}
				} else if network == "lan" {
					belly = RGB{255, 165, 0}
				}
				if err := e.hw.PulseLed(ctx, 4, belly[0], belly[1], belly[2]); err != nil {
					return err
				}
			}
			if indicator != nil {
				return e.animation(ctx, *indicator, 0)
			}
			if asleep || len(infos) == 0 {
				<-ctx.Done()
				return ctx.Err()
			}
			for {
				for _, v := range infos {
					if err := e.animation(ctx, v.animation, 15*time.Second); err != nil {
						return err
					}
				}
			}
		}
		return Result{}, 0, 0, run()
	})
}
func (e *Engine) completed(done taskResult) {
	task := e.task
	if task == nil || done.kind != task.kind {
		return
	}
	e.task = nil
	task.cancel()
	err := errors.Join(done.err, done.cleanup)
	if done.cleanup != nil {
		e.failed = true
	}
	if task.request != nil {
		result := done.result
		if done.cleanup != nil {
			result = Result{Status: "error", Error: done.cleanup.Error()}
		} else if err != nil {
			result = failure(err)
		} else if result.Status == "" {
			result.Status = "ok"
		}
		finish(task.request, result)
		for _, r := range e.cancelWaiters {
			if done.cleanup != nil {
				finish(r, failure(done.cleanup))
			} else {
				finish(r, Result{Status: "ok"})
			}
		}
		e.cancelWaiters = nil
		e.refresh = true
		e.transition = true
		e.transitionVersion++
	}
	if task.kind == "maintenance" {
		for _, m := range e.controls {
			m.reply <- err
		}
		e.controls = nil
	}
	if task.kind == "detect" && err == nil {
		var l, r *uint8
		if done.left >= 0 && done.left <= 16 {
			v := uint8(done.left)
			e.left = v
			l = &v
		}
		if done.right >= 0 && done.right <= 16 {
			v := uint8(done.right)
			e.right = v
			r = &v
		}
		if e.handlers.OnEvent != nil {
			e.handlers.OnEvent(Event{generation: e.hw.Generation(), Kind: "ears", Left: l, Right: r})
		}
		e.refresh = true
	}
	if e.state == "playing" && e.blocked {
		e.state = "idle"
	}
	e.publish()
}
