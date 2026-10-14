/* SPDX-License-Identifier: GPL-2.0-only */
#ifndef NABOS_RECOVERY_FSM_H
#define NABOS_RECOVERY_FSM_H

#define NABOS_DEBOUNCE_MS 10UL
#define NABOS_HOLD_MS 10000UL
#define NABOS_WINDOW_MS 5000UL
#define NABOS_CLICK_MS 500UL

enum nabos_phase { NABOS_INITIAL, NABOS_HOLD, NABOS_ARMED, NABOS_WINDOW, NABOS_DONE };
enum nabos_action { NABOS_WAIT, NABOS_NONE, NABOS_ROLLBACK, NABOS_RESET, NABOS_ERROR };

struct nabos_gesture {
	enum nabos_phase phase;
	enum nabos_action action;
	unsigned long previous, raw_since, hold_since, window_since;
	unsigned long press_since, release_since;
	unsigned int clicks;
	int raw, stable, activity, invalid;
};

static inline void nabos_finish(struct nabos_gesture *g, enum nabos_action action)
{
	g->phase = NABOS_DONE;
	g->action = action;
}

static inline void nabos_gesture_init(struct nabos_gesture *g,
				    unsigned long now, int pressed)
{
	*g = (struct nabos_gesture) {
		.phase = NABOS_INITIAL, .action = NABOS_WAIT,
		.previous = now, .raw_since = now, .raw = pressed, .stable = -1,
	};
	if (!pressed)
		nabos_finish(g, NABOS_NONE);
	else if (pressed != 1)
		nabos_finish(g, NABOS_ERROR);
}

/* Unsigned elapsed times also handle the ARM32 millisecond timer wrapping. */
static inline enum nabos_action nabos_gesture_step(struct nabos_gesture *g,
						unsigned long now, int pressed)
{
	unsigned long edge;

	if (g->phase == NABOS_DONE)
		return g->action;
	if ((pressed != 0 && pressed != 1) ||
	    now - g->previous > NABOS_DEBOUNCE_MS) {
		nabos_finish(g, NABOS_ERROR);
		return g->action;
	}
	g->previous = now;
	if (pressed != g->raw) {
		g->raw = pressed;
		g->raw_since = now;
		if (g->phase == NABOS_WINDOW && pressed && !g->stable) {
			g->activity = 1;
			if (g->clicks == 2)
				g->invalid = 1;
		}
	}
	if (g->phase == NABOS_DONE)
		return g->action;
	if (g->raw != g->stable && now - g->raw_since >= NABOS_DEBOUNCE_MS) {
		edge = g->raw_since;
		g->stable = g->raw;
		switch (g->phase) {
		case NABOS_INITIAL:
			if (!g->stable)
				nabos_finish(g, NABOS_NONE);
			else {
				g->phase = NABOS_HOLD;
				g->hold_since = now;
			}
			break;
		case NABOS_HOLD:
			if (!g->stable)
				nabos_finish(g, NABOS_NONE);
			break;
		case NABOS_ARMED:
			if (!g->stable) {
				g->phase = NABOS_WINDOW;
				g->window_since = now;
			}
			break;
		case NABOS_WINDOW:
			if (g->stable) {
				g->activity = 0;
				if (g->clicks && edge - g->release_since > NABOS_CLICK_MS)
					g->invalid = 1;
				g->press_since = edge;
			} else {
				if (edge - g->press_since > NABOS_CLICK_MS)
					g->invalid = 1;
				else {
					g->clicks++;
					g->release_since = edge;
				}
			}
			break;
		case NABOS_DONE:
			break;
		}
	}
	if (g->phase == NABOS_HOLD && g->raw && g->stable &&
	    now - g->hold_since >= NABOS_HOLD_MS)
		g->phase = NABOS_ARMED;
	if (g->phase == NABOS_WINDOW) {
		/* Allow the release's debounce, but measure click bounds at raw edges. */
		if ((g->stable && g->raw && now - g->press_since > NABOS_CLICK_MS) ||
		    (g->clicks == 1 && !g->stable && !g->raw &&
		     now - g->release_since > NABOS_CLICK_MS) ||
		    (g->activity && !g->raw && !g->stable &&
		     now - g->raw_since >= NABOS_DEBOUNCE_MS))
			g->invalid = 1;
		if (now - g->window_since >= NABOS_WINDOW_MS) {
			if (g->invalid || g->activity || g->raw != g->stable || g->stable ||
			    (g->clicks != 0 && g->clicks != 2))
				nabos_finish(g, NABOS_ERROR);
			else
				nabos_finish(g, g->clicks == 2 ? NABOS_RESET : NABOS_ROLLBACK);
		}
	}
	return g->action;
}
#endif
