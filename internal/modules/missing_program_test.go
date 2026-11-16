package modules

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"
)

// Every tool module that runs a program names it when it is missing, and says
// what provides it on each package manager that has it. One table covers the
// wording; each module's own test checks that the module uses it.
func TestMissingProgramNamesTheProgramAndWhatProvidesItPerPackageManager(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		program string
		want    string
	}{
		{"git", "git is not installed; install the package that provides it in an earlier node task: git (apt, pacman, zypper), git-core (dnf)"},
		{"ssh-keygen", "openssh-client (apt), openssh (dnf, pacman), openssh-clients (zypper)"},
		{"setcap", "libcap2-bin (apt), libcap (dnf, pacman), libcap-progs (zypper)"},
		{"ufw", "ufw (apt, pacman, zypper)"},
		{"update-alternatives", "dpkg (apt)"},
		{"mmx-no-such-program", "mmx-no-such-program is not installed; install the package that provides it in an earlier node task"},
	} {
		if got := missingProgramError(tc.program).Error(); !strings.Contains(got, tc.want) {
			t.Errorf("%s: %q does not contain %q", tc.program, got, tc.want)
		}
	}
}

// A package of every program that a module looks up with lookPath is in the
// table; the programs a module runs by name fall back to the same wording.
func TestAProgramThatIsNotOnThePathFailsWithTheSameWording(t *testing.T) {
	t.Parallel()
	p := runProcess(context.Background(), nil, "/", nil, nil, "getfacl-that-does-not-exist")
	if p.Started || !strings.Contains(p.Result.Reason, "getfacl-that-does-not-exist is not installed") {
		t.Fatalf("%+v", p.Result)
	}
	if err := (programNotFound{name: "git", cause: exec.ErrNotFound}); !errors.Is(err, exec.ErrNotFound) {
		t.Fatal("the cause is hidden")
	}
}

// Every entry of the table gives a message that names each package manager
// that has a package for the program, and no other manager.
func TestEveryTableEntryNamesItsPackagesInTheMessage(t *testing.T) {
	t.Parallel()
	for program, packages := range programPackages {
		message := missingProgramError(program).Error()
		for manager, name := range packages {
			if !slices.Contains(packageManagerNames, manager) {
				t.Errorf("%s: %q is not a package manager that MMX knows", program, manager)
			}
			if !strings.Contains(message, name) {
				t.Errorf("%s: the message %q leaves out %s, the package for %s", program, message, name, manager)
			}
		}
		if len(packages) == 0 {
			t.Errorf("%s has an entry without a package", program)
		}
	}
}
