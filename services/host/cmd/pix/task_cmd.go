// task_cmd.go — the argv seam for `pix task`. It exists to hold the two
// retired subcommands: `harvest` and `gc` were the launcher deciding how a
// task clone rejoins its parent repo and when it is disposable, which is git's
// job and the user's call. The rest of the verb is forwarded verbatim.
package main

import "pix/host/workflow/launch"

func runTaskCmd(argv []string) {
	if len(argv) > 0 {
		retiredIfRetired("task", argv[0])
	}
	launch.RunTask(argv)
}
