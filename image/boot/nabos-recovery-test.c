/* SPDX-License-Identifier: GPL-2.0-only */
#include <stdio.h>
#include <limits.h>
#include "nabos-recovery-fsm.h"
#include "ws2812-encode.h"

#define CHECK(expr) do { if (!(expr)) { \
	printf("FAIL line %d: %s\n", __LINE__, #expr); return 1; \
} } while (0)

static void ticks(struct nabos_gesture *g, unsigned long *now,
		  unsigned long count, int pressed)
{
	while (count--)
		nabos_gesture_step(g, ++*now, pressed);
}

static void window(struct nabos_gesture *g, unsigned long *now)
{
	*now = 0;
	nabos_gesture_init(g, *now, 1);
	ticks(g, now, 10010, 1);
	ticks(g, now, 11, 0);
}

static void finish_window(struct nabos_gesture *g, unsigned long *now)
{
	if (g->phase == NABOS_WINDOW)
		ticks(g, now, NABOS_WINDOW_MS - (*now - g->window_since), 0);
}

int nabos_recovery_selftest(void)
{
	struct nabos_gesture g;
	unsigned long now, end;
	u32 words[2 * WS2812_DATA_WORDS];
	u8 rgb[WS2812_LEDS][3] = { { 0 } };
	unsigned int i, pos, symbol, byte, bit;

	nabos_gesture_init(&g, 0, 0);
	CHECK(g.action == NABOS_NONE && g.phase == NABOS_DONE);
	nabos_gesture_init(&g, 0, -1);
	CHECK(g.action == NABOS_ERROR);
	nabos_gesture_init(&g, now = 0, 1);
	ticks(&g, &now, 9, 1);
	CHECK(g.phase == NABOS_INITIAL);
	ticks(&g, &now, 1, 1);
	CHECK(g.phase == NABOS_HOLD);
	ticks(&g, &now, 9999, 1);
	CHECK(g.phase == NABOS_HOLD);
	ticks(&g, &now, 1, 1);
	CHECK(g.phase == NABOS_ARMED && g.action == NABOS_WAIT);
	ticks(&g, &now, 30000, 1);
	CHECK(g.phase == NABOS_ARMED && g.action == NABOS_WAIT);
	ticks(&g, &now, 11, 0);
	CHECK(g.phase == NABOS_WINDOW);
	ticks(&g, &now, 4999, 0);
	CHECK(g.action == NABOS_WAIT);
	ticks(&g, &now, 1, 0);
	CHECK(g.action == NABOS_ROLLBACK);

	/* A release just before ten seconds cannot arm while it debounces. */
	nabos_gesture_init(&g, now = 0, 1);
	ticks(&g, &now, 10009, 1);
	ticks(&g, &now, 11, 0);
	CHECK(g.action == NABOS_NONE);

	/* Bounce on initial press and on release never creates clicks. */
	nabos_gesture_init(&g, now = 0, 1);
	ticks(&g, &now, 3, 1); ticks(&g, &now, 3, 0);
	ticks(&g, &now, 10011, 1);
	CHECK(g.phase == NABOS_ARMED);
	ticks(&g, &now, 3, 0); ticks(&g, &now, 3, 1);
	ticks(&g, &now, 11, 0);
	CHECK(g.phase == NABOS_WINDOW && !g.clicks);
	ticks(&g, &now, 5000, 0);
	CHECK(g.action == NABOS_ROLLBACK);

	/* Exactly inclusive 500ms presses and gap; no early reset. */
	window(&g, &now); end = now + 5000;
	ticks(&g, &now, 500, 1); ticks(&g, &now, 500, 0);
	ticks(&g, &now, 500, 1); ticks(&g, &now, 11, 0);
	CHECK(g.clicks == 2 && g.action == NABOS_WAIT);
	ticks(&g, &now, end - now - 1, 0);
	CHECK(g.action == NABOS_WAIT);
	ticks(&g, &now, 1, 0);
	CHECK(g.action == NABOS_RESET);

	/* Late valid clicks, including bounce on press/release, still use the full window. */
	window(&g, &now); end = now + 5000;
	ticks(&g, &now, 3800, 0);
	for (i = 0; i < 2; i++) {
		ticks(&g, &now, 3, 1); ticks(&g, &now, 3, 0);
		ticks(&g, &now, 20, 1);
		ticks(&g, &now, 3, 0); ticks(&g, &now, 3, 1);
		ticks(&g, &now, 20, 0);
	}
	CHECK(g.clicks == 2 && g.action == NABOS_WAIT);
	ticks(&g, &now, end - now, 0);
	CHECK(g.action == NABOS_RESET);

	window(&g, &now);
	ticks(&g, &now, 501, 1); ticks(&g, &now, 11, 0);
	CHECK(g.invalid && g.action == NABOS_WAIT);
	finish_window(&g, &now);
	CHECK(g.action == NABOS_ERROR);
	window(&g, &now);
	ticks(&g, &now, 20, 1); ticks(&g, &now, 501, 0);
	ticks(&g, &now, 20, 1); ticks(&g, &now, 11, 0);
	finish_window(&g, &now);
	CHECK(g.action == NABOS_ERROR);
	window(&g, &now);
	ticks(&g, &now, 20, 1); ticks(&g, &now, 5000, 0);
	CHECK(g.action == NABOS_ERROR);
	window(&g, &now);
	ticks(&g, &now, 4990, 0); ticks(&g, &now, 10, 1);
	CHECK(g.action == NABOS_ERROR);
	window(&g, &now);
	ticks(&g, &now, 3, 1); ticks(&g, &now, 5000, 0);
	CHECK(g.action == NABOS_ERROR);
	window(&g, &now);
	for (i = 0; i < 2; i++) {
		ticks(&g, &now, 20, 1); ticks(&g, &now, 20, 0);
	}
	ticks(&g, &now, 1, 1);
	CHECK(g.invalid && g.action == NABOS_WAIT);
	finish_window(&g, &now);
	CHECK(g.action == NABOS_ERROR);

	/* An isolated sub-debounce pulse before two valid clicks remains uncertain. */
	window(&g, &now);
	ticks(&g, &now, 3, 1); ticks(&g, &now, 20, 0);
	for (i = 0; i < 2; i++) {
		ticks(&g, &now, 20, 1); ticks(&g, &now, 20, 0);
	}
	finish_window(&g, &now);
	CHECK(g.action == NABOS_ERROR);

	/* Sampling gaps, backwards time and read failures fail closed. */
	for (i = 0; i < 4; i++) {
		unsigned int failure;

		for (failure = 0; failure < 3; failure++) {
			nabos_gesture_init(&g, now = 0, 1);
			if (i > 0) ticks(&g, &now, 10, 1);
			if (i > 1) ticks(&g, &now, 10000, 1);
			if (i > 2) ticks(&g, &now, 11, 0);
			nabos_gesture_step(&g, failure == 0 ? now + 11 :
					   failure == 1 ? now - 1 : now + 1,
					   failure == 2 ? -1 : i == 3 ? 0 : 1);
			CHECK(g.action == NABOS_ERROR);
		}
	}
	nabos_gesture_init(&g, now = ULONG_MAX - 5, 1);
	ticks(&g, &now, 10010, 1);
	CHECK(g.phase == NABOS_ARMED);

	/* Decode every symbol: all five LEDs, GRB, MSB first, idle serializer zero. */
	for (i = 0; i < WS2812_LEDS; i++) {
		rgb[i][0] = 0xa5; rgb[i][1] = 0x3c; rgb[i][2] = 0x81;
	}
	ws2812_encode(words, rgb);
	for (i = 0; i < WS2812_DATA_WORDS; i++)
		CHECK(words[2 * i] == 0);
	for (pos = 0; pos < WS2812_DATA_BITS; pos++) {
		static const unsigned int grb[] = { 1, 0, 2 };
		symbol = pos % 3; byte = pos / 24; bit = 7 - (pos / 3) % 8;
		CHECK(((words[2 * (pos / 32) + 1] >> (31 - pos % 32)) & 1) ==
		      (unsigned int)(!symbol || (symbol == 1 &&
					(rgb[byte / 3][grb[byte % 3]] & (1U << bit)))));
	}
	CHECK((words[2 * WS2812_DATA_WORDS - 1] & 0xffffff) == 0);
	puts("PASS: recovery debounce/10s/30s/5s/click bounds/bounce/failures/timer wrap; five-LED GRB dual-serializer encoding");
	return 0;
}

#ifndef __UBOOT__
int main(void) { return nabos_recovery_selftest(); }
#endif
