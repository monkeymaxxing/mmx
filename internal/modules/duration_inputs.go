package modules

import (
	"strings"
	"time"
)

// DurationInputs is implemented by module inputs whose durations may be one
// whole reference to a var, such as wait_at_most: "{{ .vars.reboot_wait }}".
// MMX puts each node's value in before the run (config.ResolveDurations), so
// the controller process and the node read only written durations. visit
// gets each input's name below the module's name, its text, and the check
// the duration must pass.
type DurationInputs interface {
	DurationInputs(visit func(field string, text *string, check func(string) error))
}

// writtenDuration reports a duration that is written out and holds no
// reference. A reference is checked as the project loads by
// config.ResolveDurations, which also explains one that MMX cannot take.
func writtenDuration(text string) bool { return !strings.Contains(text, "{{") }

func (r *Reboot) DurationInputs(visit func(string, *string, func(string) error)) {
	visit("wait_at_most", &r.WaitAtMost, func(s string) error { _, err := Reboot{WaitAtMost: s}.Wait(); return err })
}

func (w *WaitFor) DurationInputs(visit func(string, *string, func(string) error)) {
	visit("wait_at_most", &w.WaitAtMost, func(s string) error { _, err := WaitFor{WaitAtMost: s}.Wait(); return err })
}

func (j *SystemdScheduledJob) DurationInputs(visit func(string, *string, func(string) error)) {
	visit("random_delay", &j.RandomDelay, scheduledJobDuration("random_delay"))
	visit("stop_a_run_after", &j.StopARunAfter, scheduledJobDuration("stop_a_run_after"))
}

// scheduledJobDuration is the check of a systemd_scheduled_job duration.
func scheduledJobDuration(field string) func(string) error {
	return func(text string) error {
		if d, err := time.ParseDuration(text); err != nil || d <= 0 {
			return invalid(field, "systemd_scheduled_job."+field+" must be a positive duration, such as 90s, 15m, or 2h")
		}
		return nil
	}
}

// confirmationWithin is the check of restore_unless_controller_confirms_within.
func confirmationWithin(text string) error {
	_, err := NetworkFile{RestoreUnlessControllerConfirmsWithin: text}.confirmationWait()
	return err
}

func (n *NetplanFile) DurationInputs(visit func(string, *string, func(string) error)) {
	visit("restore_unless_controller_confirms_within", &n.RestoreUnlessControllerConfirmsWithin, confirmationWithin)
}

func (n *SystemdNetworkdFile) DurationInputs(visit func(string, *string, func(string) error)) {
	visit("restore_unless_controller_confirms_within", &n.RestoreUnlessControllerConfirmsWithin, confirmationWithin)
}

func (n *IfupdownInterfacesFile) DurationInputs(visit func(string, *string, func(string) error)) {
	visit("restore_unless_controller_confirms_within", &n.RestoreUnlessControllerConfirmsWithin, confirmationWithin)
}

func (w *WireguardInterface) DurationInputs(visit func(string, *string, func(string) error)) {
	visit("restore_unless_controller_confirms_within", &w.RestoreUnlessControllerConfirmsWithin, confirmationWithin)
}

func (n *NetworkManagerConnectionFile) DurationInputs(visit func(string, *string, func(string) error)) {
	visit("restore_unless_controller_confirms_within", &n.RestoreUnlessControllerConfirmsWithin, confirmationWithin)
}

func (p *Package) DurationInputs(visit func(string, *string, func(string) error)) {
	visit("wait_for_apt_lock_at_most", &p.WaitForAptLockAtMost, func(s string) error { _, err := aptLockWait(p.Module(), s); return err })
}

func (a *AptDownloadedPackageFiles) DurationInputs(visit func(string, *string, func(string) error)) {
	visit("wait_for_apt_lock_at_most", &a.WaitForAptLockAtMost, func(s string) error {
		_, err := aptLockWait("apt_downloaded_package_files", s)
		return err
	})
}
