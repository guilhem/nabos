// SPDX-License-Identifier: GPL-2.0-only
#include <command.h>
#include <env.h>
#include <errno.h>
#include <stdio.h>
#include <string.h>
#include <time.h>
#include <linux/delay.h>
#include <u-boot/schedule.h>
#include "nabos-recovery-fsm.h"
#include "ws2812-encode.h"

#ifndef CONFIG_SANDBOX
#include <asm/gpio.h>
#include <asm/io.h>
#include <asm/arch/base.h>

#define GPIO_FSEL1 0x04
#define GPIO_CLR0 0x28
#define GPIO_LEVEL0 0x34
#define GPIO_PUD 0x94
#define GPIO_PUDCLK0 0x98
#define PWM_CTL 0x00
#define PWM_STA 0x04
#define PWM_DMAC 0x08
#define PWM_RNG1 0x10
#define PWM_FIFO 0x18
#define PWM_RNG2 0x20
#define PWM_MODE ((1U << 1) | (1U << 5) | (1U << 9) | (1U << 13))
#define PWM_ENABLE ((1U << 0) | (1U << 8))
#define PWM_CLEAR (1U << 6)
#define PWM_FULL (1U << 0)
#define PWM_EMPTY (1U << 1)
#define PWM_GAPS ((1U << 4) | (1U << 5))
#define PWM_ERRORS ((1U << 2) | (1U << 3) | (1U << 8))
#define CM_CTL 0xa0
#define CM_DIV 0xa4
#define CM_PASSWORD 0x5a000000U
#define CM_BUSY (1U << 7)
#define CM_ENABLE (1U << 4)
#define FIFO_TIMEOUT_US 5000UL

static void __iomem *gpio, *pwm, *clock;

static int nabos_hw_init(void)
{
	unsigned long base = rpi_timer_base - 0x3000;

	/* Only the two NabOS SoCs have this GPIO pull and 19.2MHz oscillator. */
	if (base != 0x20000000 && base != 0x3f000000)
		return -ENODEV;
	gpio = (void __iomem *)(base + 0x200000);
	pwm = (void __iomem *)(base + 0x20c000);
	clock = (void __iomem *)(base + 0x101000);
	return 0;
}

static int nabos_clock_wait(int busy)
{
	unsigned long start = timer_get_us();

	while (!!(readl(clock + CM_CTL) & CM_BUSY) != busy) {
		if (timer_get_us() - start >= FIFO_TIMEOUT_US)
			return -ETIMEDOUT;
		schedule();
	}
	return 0;
}

static int nabos_led_stop(void)
{
	u32 status;
	int ret;

	/* No DMA channel is started: disable PWM DREQ as well as both serializers. */
	writel(0, pwm + PWM_DMAC);
	writel(PWM_MODE | PWM_CLEAR, pwm + PWM_CTL);
	readl(pwm + PWM_CTL);
	udelay(300);
	status = readl(pwm + PWM_STA);
	writel(PWM_ERRORS | PWM_GAPS, pwm + PWM_STA);
	writel(1U << 13, gpio + GPIO_CLR0);
	clrsetbits_le32(gpio + GPIO_FSEL1, 7U << 9, 1U << 9);
	writel(CM_PASSWORD | 1, clock + CM_CTL);
	ret = nabos_clock_wait(0);
	if ((readl(pwm + PWM_CTL) & PWM_ENABLE) || readl(pwm + PWM_DMAC) ||
	    !(readl(pwm + PWM_STA) & PWM_EMPTY) ||
	    (readl(gpio + GPIO_LEVEL0) & (1U << 13)) ||
	    (status & PWM_ERRORS))
		return -EIO;
	return ret;
}

static int nabos_led_rgb(u8 red, u8 green, u8 blue)
{
	u32 words[2 * (WS2812_DATA_WORDS + 23)] = { 0 };
	u8 rgb[WS2812_LEDS][3];
	unsigned long start;
	unsigned int i;
	u32 status;
	int ret, stop_ret;

	ret = nabos_hw_init();
	if (ret)
		return ret;
	ret = nabos_led_stop();
	if (ret)
		return ret;
	/* OSC / 8 = 2.4MHz; no fractional divider or PLL parent. */
	writel(CM_PASSWORD | (8U << 12), clock + CM_DIV);
	writel(CM_PASSWORD | CM_ENABLE | 1, clock + CM_CTL);
	ret = nabos_clock_wait(1);
	if (ret)
		goto stop;
	if ((readl(clock + CM_DIV) & 0xffffff) != (8U << 12) ||
	    (readl(clock + CM_CTL) & 0x1f) != (CM_ENABLE | 1)) {
		ret = -EIO;
		goto stop;
	}
	clrsetbits_le32(gpio + GPIO_FSEL1, 7U << 9, 4U << 9);
	if ((readl(gpio + GPIO_FSEL1) & (7U << 9)) != (4U << 9)) {
		ret = -EIO;
		goto stop;
	}
	for (i = 0; i < WS2812_LEDS; i++) {
		rgb[i][0] = red; rgb[i][1] = green; rgb[i][2] = blue;
	}
	/* Encoder copied from locked bcm2835-ws2812 2b1adcea38ad. */
	ws2812_encode(words, rgb);
	writel(32, pwm + PWM_RNG1);
	writel(32, pwm + PWM_RNG2);
	/* Match the pinned driver: FIFO consumers configured before clear/prefill. */
	writel(PWM_MODE | PWM_CLEAR, pwm + PWM_CTL);
	readl(pwm + PWM_CTL);
	for (i = 0; i < 16; i++)
		writel(0, pwm + PWM_FIFO);
	writel(PWM_MODE | PWM_ENABLE, pwm + PWM_CTL);
	start = timer_get_us();
	/* The 23 paired zero suffix words provide >=300us of low latch time. */
	for (i = 0; i < sizeof(words) / sizeof(words[0]); i++) {
		do {
			status = readl(pwm + PWM_STA);
			if (status & (PWM_ERRORS | PWM_GAPS)) {
				ret = -EIO;
				goto stop;
			}
			if (timer_get_us() - start >= FIFO_TIMEOUT_US) {
				ret = -ETIMEDOUT;
				goto stop;
			}
		} while (status & PWM_FULL);
		writel(words[i], pwm + PWM_FIFO);
	}
	do {
		status = readl(pwm + PWM_STA);
		if (status & PWM_ERRORS) {
			ret = -EIO;
			goto stop;
		}
		if (timer_get_us() - start >= FIFO_TIMEOUT_US) {
			ret = -ETIMEDOUT;
			goto stop;
		}
	} while (!(status & PWM_EMPTY));
	/* EMPTY is entry into the last shifter, not completion at GPIO13. */
	udelay(14);
stop:
	stop_ret = nabos_led_stop();
	return ret ? ret : stop_ret;
}

static int nabos_button_init(void)
{
	int ret = nabos_hw_init();

	if (ret)
		return ret;
	ret = gpio_request(17, "nabos-recovery");
	if (ret)
		return ret;
	ret = gpio_direction_input(17);
	if (ret) {
		gpio_free(17);
		return ret;
	}
	/* Legacy BCM2835/2837 pull sequence, explicitly pulling GPIO17 high. */
	writel(2, gpio + GPIO_PUD);
	udelay(5);
	writel(1U << 17, gpio + GPIO_PUDCLK0);
	udelay(5);
	writel(0, gpio + GPIO_PUD);
	writel(0, gpio + GPIO_PUDCLK0);
	if (readl(gpio + GPIO_FSEL1) & (7U << 21)) {
		gpio_free(17);
		return -EIO;
	}
	return 0;
}

static int nabos_pressed(void)
{
	int value;

	if (readl(gpio + GPIO_FSEL1) & (7U << 21))
		return -EIO;
	value = gpio_get_value(17);
	return value < 0 ? value : !value;
}
#else
static int nabos_led_rgb(u8 red, u8 green, u8 blue)
{
	/* Sandbox exercises the FSM, and must never claim GPIO/FIFO qualification. */
	return -ENOSYS;
}
#endif

static int nabos_led_fail(void)
{
	unsigned int i;
	int ret;

	for (i = 0; i < 3; i++) {
		ret = nabos_led_rgb(255, 0, 0);
		if (ret)
			return ret;
		mdelay(150);
		ret = nabos_led_rgb(0, 0, 0);
		if (ret)
			return ret;
		mdelay(150);
	}
	return 0;
}

/* Called by RPi misc_init_r, after env relocation and before any boot script. */
int nabos_recovery_init(void)
{
	int ret = env_set("nabos_recovery_action", "none");
#ifndef CONFIG_SANDBOX
	int button_requested = 0;
#endif

	/* A stale saved action must not survive failure to overwrite it. */
	if (ret) {
		nabos_led_fail();
		return ret;
	}
	if (env_get("NABOS_RESET_SLOT") && *env_get("NABOS_RESET_SLOT"))
		return 0;
#ifndef CONFIG_SANDBOX
	{
		struct nabos_gesture g;
		unsigned long now;
		int feedback = -1, next;

		ret = nabos_button_init();
		if (ret)
			goto fail;
		button_requested = 1;
		nabos_gesture_init(&g, get_timer(0), nabos_pressed());
		while (g.action == NABOS_WAIT) {
			/* Feeds only already-running watchdogs via their cyclic callbacks. */
			schedule();
			now = get_timer(0);
			nabos_gesture_step(&g, now, nabos_pressed());
			next = g.phase == NABOS_ARMED ? 1 :
				g.phase == NABOS_WINDOW ? !((now - g.window_since) / 250 % 2) : 0;
			if ((g.phase == NABOS_ARMED || g.phase == NABOS_WINDOW) &&
			    next != feedback) {
				ret = nabos_led_rgb(next ? 255 : 0, next ? 96 : 0, 0);
				if (ret)
					goto fail;
				feedback = next;
			}
			udelay(1000);
		}
		ret = gpio_free(17);
		button_requested = 0;
		if (ret)
			goto fail;
		if (g.action == NABOS_ERROR)
			goto fail;
		if (g.action == NABOS_ROLLBACK || g.action == NABOS_RESET) {
			/* boot.cmd acknowledges rollback in green only after persistence. */
			ret = nabos_led_rgb(255, g.action == NABOS_RESET ? 0 : 96, 0);
			if (ret)
				goto fail;
			ret = env_set("nabos_recovery_action",
				      g.action == NABOS_RESET ? "reset" : "rollback");
			if (ret)
				goto fail;
		}
	}
	return 0;
fail:
	puts("nabos: physical recovery cancelled\n");
	if (button_requested)
		gpio_free(17);
	/* env_set can lose its old value on ENOMEM; refuse boot if clearing fails. */
	ret = env_set("nabos_recovery_action", "none");
	nabos_led_fail();
	if (ret)
		return ret;
#endif
	return 0;
}

static int do_nabos_led(struct cmd_tbl *cmdtp, int flag, int argc, char *const argv[])
{
	int ret;

	if (argc != 2)
		return CMD_RET_USAGE;
	if (!strcmp(argv[1], "orange"))
		ret = nabos_led_rgb(255, 96, 0);
	else if (!strcmp(argv[1], "green"))
		ret = nabos_led_rgb(0, 255, 0);
	else if (!strcmp(argv[1], "red"))
		ret = nabos_led_rgb(255, 0, 0);
	else if (!strcmp(argv[1], "off"))
		ret = nabos_led_rgb(0, 0, 0);
	else if (!strcmp(argv[1], "fail"))
		ret = nabos_led_fail();
	else
		return CMD_RET_USAGE;
	if (ret)
		printf("nabos_led: feedback failed (%d)\n", ret);
	return ret ? CMD_RET_FAILURE : CMD_RET_SUCCESS;
}

U_BOOT_CMD(nabos_led, 2, 0, do_nabos_led, "NabOS recovery feedback",
	   "orange|green|red|off|fail");

#ifdef CONFIG_SANDBOX
int nabos_recovery_selftest(void);

static int do_nabos_recovery_test(struct cmd_tbl *cmdtp, int flag,
				int argc, char *const argv[])
{
	if (nabos_recovery_selftest() ||
	    env_set("nabos_recovery_action", "reset") || nabos_recovery_init() ||
	    !env_get("nabos_recovery_action") ||
	    strcmp(env_get("nabos_recovery_action"), "none"))
		return CMD_RET_FAILURE;
	if (env_set("NABOS_RESET_SLOT", "A") ||
	    env_set("nabos_recovery_action", "rollback") || nabos_recovery_init() ||
	    !env_get("nabos_recovery_action") ||
	    strcmp(env_get("nabos_recovery_action"), "none") ||
	    strcmp(env_get("NABOS_RESET_SLOT"), "A") || env_set("NABOS_RESET_SLOT", NULL))
		return CMD_RET_FAILURE;
	puts("PASS: stale actions overwritten; pending reset preserved (sandbox, no hardware)\n");
	return CMD_RET_SUCCESS;
}

U_BOOT_CMD(nabos_recovery_test, 1, 0, do_nabos_recovery_test,
	   "run shared native recovery tests (no hardware)", "");
#endif
