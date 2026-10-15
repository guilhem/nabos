// SPDX-License-Identifier: GPL-2.0-only
/* Failure injection in the live control DT; native MMC/RAUC stay unmodified. */
#include <command.h>
#include <dm/ofnode.h>
#include <mmc.h>
#include <string.h>

static int do_nabos_env_test(struct cmd_tbl *cmdtp, int flag, int argc,
			   char *const argv[])
{
	ofnode config = ofnode_path("/config");
	struct mmc *mmc = find_mmc_device(CONFIG_ENV_MMC_DEVICE_INDEX);
	u32 first, second;
	extern int nabos_recovery_init(void);

	if (argc == 2 && !strcmp(argv[1], "prepare"))
		return nabos_recovery_init() ? CMD_RET_FAILURE : CMD_RET_SUCCESS;
	if (argc != 2 || !ofnode_valid(config) || !mmc)
		return CMD_RET_USAGE;
	if (!strcmp(argv[1], "fail")) {
		first = mmc->capacity;
		second = first + CONFIG_ENV_SIZE;
	} else if (!strcmp(argv[1], "restore")) {
		first = CONFIG_ENV_OFFSET;
		second = CONFIG_ENV_OFFSET_REDUND;
	} else {
		return CMD_RET_USAGE;
	}
	if (ofnode_write_u32(config, "u-boot,mmc-env-offset", first) ||
	    ofnode_write_u32(config, "u-boot,mmc-env-offset-redundant", second))
		return CMD_RET_FAILURE;
	return CMD_RET_SUCCESS;
}

U_BOOT_CMD(nabos_env_test, 2, 0, do_nabos_env_test,
	   "sandbox MMC environment offsets", "prepare|fail|restore");
