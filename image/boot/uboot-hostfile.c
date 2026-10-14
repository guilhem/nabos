// SPDX-License-Identifier: GPL-2.0-only
/* Native sandbox persistence only; no claim about MMC redundancy/durability. */
#include <env.h>
#include <env_internal.h>
#include <errno.h>
#include <os.h>
#include <sandboxfs.h>
#include <search.h>

static int env_hostfile_load(void)
{
	env_t env;
	loff_t size, actual;
	int ret;

	ret = os_get_filesize("uboot.env", &size);
	if (ret)
		goto fail;
	if (size != sizeof(env)) {
		ret = -EINVAL;
		goto fail;
	}
	ret = fs_read_sandbox("uboot.env", &env, 0, sizeof(env), &actual);
	if (ret || actual != sizeof(env)) {
		ret = ret ? ret : -EIO;
		goto fail;
	}
	return env_import((char *)&env, 1, H_EXTERNAL);
fail:
	env_set_default("host file unavailable", 0);
	return ret;
}

static int env_hostfile_save(void)
{
	env_t env;
	int ret = env_export(&env);

	return ret ? ret : os_write_file("uboot.env", &env, sizeof(env));
}

U_BOOT_ENV_LOCATION(hostfile) = {
	.location = ENVL_HOSTFILE,
	ENV_NAME("Host file")
	.load = env_hostfile_load,
	.save = ENV_SAVE_PTR(env_hostfile_save),
};
