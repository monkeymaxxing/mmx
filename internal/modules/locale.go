package modules

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"unicode"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// The files the locale module reads and edits. Tests point them elsewhere.
var (
	// localeGenFile lists the locales locale-gen makes on Debian and Ubuntu,
	// one "name charmap" line each; a line starting with # is off.
	localeGenFile = "/etc/locale.gen"
	// supportedLocalesFile lists every locale glibc can make there, in the
	// same form.
	supportedLocalesFile = "/usr/share/i18n/SUPPORTED"
	// debianDefaultLocaleFile holds the system default on Debian and Ubuntu,
	// which update-locale writes.
	debianDefaultLocaleFile = "/etc/default/locale"
)

// localePrograms are the programs the locale module runs on the nodes of each
// package manager; programPackages names what provides each.
var localePrograms = map[string][]string{
	"apt": {"locale", "locale-gen", "update-locale"},
	"dnf": {"locale", "localectl"},
}

// Locale makes one locale available on the node and, when the task says so,
// sets it as the system default (LANG). How depends on the node's package
// manager: apt nodes make locales with locale-gen from
// /etc/locale.gen; dnf nodes get them from glibc-langpack packages.
func (run Runtime) Locale(ctx context.Context, l Locale, packageManager string) (result protocol.Result) {
	if err := ValidateLocale(l); err != nil {
		return protocol.ConfigurationError(err)
	}
	programs, known := localePrograms[packageManager]
	if !known {
		return protocol.Failed(fmt.Errorf("locale requires apt or dnf on the node; the node's package manager is %q", packageManager))
	}
	for _, program := range programs {
		if _, err := lookPath(ctx, program); err != nil {
			return run.notYet(missingProgramError(program))
		}
	}
	// changes is what the task changes or, in a dryrun, would change; done
	// is what a run did, so that a failed task never reports a change that
	// did not happen.
	changes, done := map[string]any{}, map[string]any{}
	defer func() {
		switch {
		case run.Dryrun && len(changes) > 0 && result.Change == "would_change":
			result.Value = changes
		case !run.Dryrun && len(done) > 0:
			result.Value = done
		}
	}()
	available, err := run.localeAvailable(ctx, l.Name)
	if err != nil {
		return protocol.Failed(err)
	}
	changed := false
	failed := func(err error) protocol.Result {
		r := protocol.Failed(err)
		if changed {
			r.Change = "changed"
		}
		return r
	}
	if !available {
		if packageManager == "dnf" {
			// An earlier dnf_package task may install the package.
			return run.notYet(fmt.Errorf("locale %s is not installed; install the package %s first with dnf_package", l.Name, localeLanguagePack(l.Name)))
		}
		line, err := run.localeGenLine(l.Name)
		if err != nil {
			return protocol.ConfigurationError(err)
		}
		changes["made_available"] = true
		if !run.Dryrun {
			if line != "" {
				edit := Line{InsertAtEndOfFile: true, Path: localeGenFile, Match: `^\s*#?\s*` + regexp.QuoteMeta(l.Name) + `\s`, Text: line}
				if r := run.Line(ctx, edit, value.Scope{}); r.Status != "succeeded" {
					return protocol.Failed(fmt.Errorf("enable %q in %s: %s", line, localeGenFile, r.Reason))
				}
				changed = true
			}
			made := run.RunCommandThatChangesTheNode(ctx, []string{"locale-gen"}, "")
			changed = true
			if err := commandFailure(made, "locale-gen"); err != nil {
				return failed(err)
			}
			if available, err = run.localeAvailable(ctx, l.Name); err == nil && !available {
				err = fmt.Errorf("locale-gen completed, but locale -a still does not list %s", l.Name)
			}
			if err != nil {
				return failed(err)
			}
			done["made_available"] = true
		}
	}
	if *l.UseAsSystemDefault {
		settings, err := run.systemLocale(ctx, packageManager)
		if err != nil {
			return failed(err)
		}
		before := localeSetting(settings, "LANG")
		if before != l.Name {
			changes["system_default_before"], changes["system_default_after"] = before, l.Name
			if !run.Dryrun {
				if err := run.setSystemLocale(ctx, packageManager, settings, l.Name); err != nil {
					return failed(err)
				}
				changed = true
				settings, err := run.systemLocale(ctx, packageManager)
				if err == nil && localeSetting(settings, "LANG") != l.Name {
					err = fmt.Errorf("the system default locale is LANG=%s after the change, not %s", localeSetting(settings, "LANG"), l.Name)
				}
				if err != nil {
					return failed(err)
				}
				done["system_default_before"], done["system_default_after"] = before, l.Name
			}
		}
	}
	switch {
	case len(changes) == 0:
		return protocol.Success("unchanged")
	case run.Dryrun:
		return protocol.Success("would_change")
	}
	return protocol.Success("changed")
}

// localeAvailable says whether locale -a lists the locale. glibc lists the
// character set of a name in lower case without punctuation: en_US.UTF-8 is
// en_US.utf8.
func (run Runtime) localeAvailable(ctx context.Context, name string) (bool, error) {
	out := run.RunCommand(ctx, []string{"locale", "-a"}, "")
	if err := commandFailure(out, "locale -a"); err != nil {
		return false, err
	}
	want := normalizedLocale(name)
	for _, listed := range strings.Fields(out.StandardOutput) {
		if normalizedLocale(listed) == want {
			return true, nil
		}
	}
	return false, nil
}

func normalizedLocale(name string) string {
	name, modifier, hasModifier := strings.Cut(name, "@")
	if language, charset, ok := strings.Cut(name, "."); ok {
		charset = strings.Map(func(r rune) rune {
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				return unicode.ToLower(r)
			}
			return -1
		}, charset)
		name = language + "." + charset
	}
	if hasModifier {
		name += "@" + modifier
	}
	return name
}

// localeGenLine is the line /etc/locale.gen needs for the locale, such as
// "de_DE.UTF-8 UTF-8", or "" when an enabled line is there already. The
// character map comes from the line that is switched off in /etc/locale.gen,
// or else from /usr/share/i18n/SUPPORTED.
func (run Runtime) localeGenLine(name string) (string, error) {
	generate := run.ReadFile(localeGenFile)
	if generate.Error != "" && run.InspectPath(localeGenFile).Exists {
		return "", fmt.Errorf("read %s: %s", localeGenFile, generate.Error)
	}
	for _, line := range strings.Split(generate.Content, "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == name {
			return "", nil
		}
	}
	for _, line := range strings.Split(generate.Content, "\n") {
		off := strings.TrimSpace(line)
		if !strings.HasPrefix(off, "#") {
			continue
		}
		if fields := strings.Fields(strings.TrimPrefix(off, "#")); len(fields) == 2 && fields[0] == name {
			return name + " " + fields[1], nil
		}
	}
	supported := run.ReadFile(supportedLocalesFile)
	if supported.Error != "" {
		return "", fmt.Errorf("%s has no line for locale %s, and %s cannot be read: %s", localeGenFile, name, supportedLocalesFile, supported.Error)
	}
	for _, line := range strings.Split(supported.Content, "\n") {
		if fields := strings.Fields(line); len(fields) == 2 && fields[0] == name {
			return name + " " + fields[1], nil
		}
	}
	return "", fmt.Errorf("this node cannot make locale %s: neither %s nor %s lists it; check the spelling, such as en_US.UTF-8", name, localeGenFile, supportedLocalesFile)
}

// localeLanguagePack is the Red Hat family's package for a locale's language:
// glibc-langpack-de for de_DE.UTF-8.
func localeLanguagePack(name string) string {
	language := strings.FieldsFunc(name, func(r rune) bool { return r == '_' || r == '.' || r == '@' })[0]
	return "glibc-langpack-" + language
}

// localeSettingLine is one VARIABLE=value setting of the system locale.
var localeSettingLine = regexp.MustCompile(`^([A-Z_]+)=(.*)$`)

// systemLocale is the system locale's settings in order, such as
// ["LANG=en_US.UTF-8", "LC_TIME=de_DE.UTF-8"]: on apt nodes from
// /etc/default/locale, which update-locale writes, and on dnf nodes from
// localectl.
func (run Runtime) systemLocale(ctx context.Context, packageManager string) ([]string, error) {
	var lines []string
	if packageManager == "apt" {
		file := run.ReadFile(debianDefaultLocaleFile)
		if file.Error != "" && run.InspectPath(debianDefaultLocaleFile).Exists {
			return nil, fmt.Errorf("read %s: %s", debianDefaultLocaleFile, file.Error)
		}
		lines = strings.Split(file.Content, "\n")
	} else {
		out := run.RunCommand(ctx, []string{"localectl", "status"}, "")
		if err := commandFailure(out, "localectl status"); err != nil {
			return nil, err
		}
		// "   System Locale: LANG=en_US.UTF-8" and one indented line for
		// each further setting, up to the next "Name: value" line.
		in := false
		for _, line := range strings.Split(out.StandardOutput, "\n") {
			line = strings.TrimSpace(line)
			if rest, ok := strings.CutPrefix(line, "System Locale:"); ok {
				in, line = true, strings.TrimSpace(rest)
			} else if strings.Contains(line, ": ") {
				in = false
			}
			if in {
				lines = append(lines, line)
			}
		}
	}
	var settings []string
	for _, line := range lines {
		if m := localeSettingLine.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			settings = append(settings, m[1]+"="+strings.Trim(m[2], `"'`))
		}
	}
	return settings, nil
}

func localeSetting(settings []string, variable string) string {
	for _, setting := range settings {
		if v, ok := strings.CutPrefix(setting, variable+"="); ok {
			return v
		}
	}
	return ""
}

// setSystemLocale sets LANG and keeps the other settings. On apt nodes
// update-locale changes only LANG; Debian 13 does not let localectl change
// the locale. localectl set-locale replaces every setting, so the others
// are given again.
func (run Runtime) setSystemLocale(ctx context.Context, packageManager string, settings []string, name string) error {
	if packageManager == "apt" {
		return commandFailure(run.RunCommandThatChangesTheNode(ctx, []string{"update-locale", "LANG=" + name}, ""), "update-locale LANG="+name)
	}
	arguments := []string{"localectl", "set-locale", "LANG=" + name}
	for _, setting := range settings {
		if !strings.HasPrefix(setting, "LANG=") {
			arguments = append(arguments, setting)
		}
	}
	return commandFailure(run.RunCommandThatChangesTheNode(ctx, arguments, ""), strings.Join(arguments, " "))
}

// Locale is one locale the node offers, and whether it is the system default.
type Locale struct {
	Name               string `yaml:"name" json:"name"`
	UseAsSystemDefault *bool  `yaml:"use_as_system_default" json:"use_as_system_default"`
}

// localeName is a locale name as glibc writes it: a language, an optional
// territory, character set, and modifier, such as en_US.UTF-8, de_DE,
// sr_RS@latin, or C.UTF-8.
var localeName = regexp.MustCompile(`^[A-Za-z]+(_[A-Za-z]+)?(\.[A-Za-z0-9-]+)?(@[A-Za-z0-9]+)?$`)

var localeRule = parsed(func(s string) error {
	if !localeName.MatchString(s) {
		return errors.New("locale.name must be a locale name such as en_US.UTF-8: a language, then optionally _ and a territory, . and a character set, and @ and a modifier")
	}
	return nil
})

func ValidateLocale(l Locale) error {
	if l.Name == "" {
		return invalid("name", "locale.name is required, such as en_US.UTF-8")
	}
	if l.UseAsSystemDefault == nil {
		return invalid("use_as_system_default", "locale.use_as_system_default is required: true to set the locale as the system default (LANG), false to leave the default alone")
	}
	return nil
}

// localeDescription is what mmx describe-module shows for locale.
var localeDescription = Description{
	Name: "locale", Topic: TopicHostNameTimeZoneLocaleAndAlternatives, Schema: reflect.TypeOf(Locale{}),
	Summary: "one locale the node offers, and whether it is the system default; write one task for each locale",
	Fields: []Field{
		{Name: "name", Required: true, Notes: "such as en_US.UTF-8; on Debian and Ubuntu a missing locale gets its line in /etc/locale.gen enabled and locale-gen makes it; on the Red Hat family a missing locale fails and names the package glibc-langpack-<language> to install first with dnf_package"},
		{Name: "use_as_system_default", Required: true, Notes: "true sets LANG of the system locale (update-locale on Debian and Ubuntu, localectl on the Red Hat family) and keeps its other settings; false leaves the system default alone"},
	},
	Example: "- locale:\n    name: en_US.UTF-8\n    use_as_system_default: true\n",
	Results: []string{"status", "change", "value.made_available", "value.system_default_before", "value.system_default_after"},
	Dryrun:  "supported; reports whether the locale would be made and the system default that would be set; a missing glibc-langpack package is not checked, because an earlier task may install it",
}

// Prepare checks the inputs when the project loads.
func (l *Locale) Prepare(base string) error {
	return ValidateLocale(*l)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (l *Locale) Arguments(arguments *ArgumentList) {
	arguments.Public("name", &l.Name, localeRule)
}

// Run does the module's work on the node, by the node's package manager.
func (l *Locale) Run(ctx context.Context, task NodeTask) protocol.Result {
	packageManager := ""
	if task.Packages != nil {
		packageManager = task.Packages.Name()
	}
	return task.Runtime.Locale(ctx, *l, packageManager)
}
