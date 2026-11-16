package modules

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"reflect"
	"regexp"
	"strings"
	"syscall"

	"mmx/internal/protocol"
)

// timeZoneDatabase is where the time zone database (package tzdata) keeps
// one file for each time zone name. Tests point it elsewhere.
var timeZoneDatabase = "/usr/share/zoneinfo"

// localTimeLink is the link that names the node's time zone.
var localTimeLink = "/etc/localtime"

// TimeZone sets the node's time zone through timedatectl. A name the time
// zone database does not have fails before any change.
func (run Runtime) TimeZone(ctx context.Context, z TimeZone) (result protocol.Result) {
	if err := ValidateTimeZone(z); err != nil {
		return protocol.ConfigurationError(err)
	}
	if _, err := lookPath(ctx, "timedatectl"); err != nil {
		return run.notYet(missingProgramError("timedatectl"))
	}
	database := run.InspectPath(timeZoneDatabase)
	if database.Error != "" {
		return protocol.Failed(fmt.Errorf("inspect %s: %s", timeZoneDatabase, database.Error))
	}
	if database.Type != "directory" {
		return run.notYet(fmt.Errorf("the time zone database %s is not installed; install the package tzdata first", timeZoneDatabase))
	}
	zone := run.InspectPath(timeZoneDatabase + "/" + z.Name)
	if zone.Error != "" {
		return protocol.Failed(fmt.Errorf("inspect %s/%s: %s", timeZoneDatabase, z.Name, zone.Error))
	}
	if zone.Type != "file" && zone.Type != "link" {
		return protocol.ConfigurationError(fmt.Errorf("%s is not a time zone in the time zone database %s; timedatectl list-timezones lists the names, such as Europe/Berlin or UTC", z.Name, timeZoneDatabase))
	}
	before, err := run.readTimeZone(ctx)
	if err != nil {
		return protocol.Failed(err)
	}
	if before == z.Name {
		return protocol.Success("unchanged")
	}
	defer func() {
		if result.Change == "changed" || result.Change == "would_change" {
			result.Value = map[string]any{"time_zone_before": before, "time_zone_after": z.Name}
		}
	}()
	if run.Dryrun {
		return protocol.Success("would_change")
	}
	set := run.RunCommandThatChangesTheNode(ctx, []string{"timedatectl", "set-timezone", z.Name}, "")
	if err := commandFailure(set, "timedatectl set-timezone "+z.Name); err != nil {
		return protocol.Failed(err)
	}
	after, err := run.readTimeZone(ctx)
	if err == nil && after != z.Name {
		err = fmt.Errorf("after timedatectl set-timezone, the node's time zone is %q, not %q", after, z.Name)
	}
	if err != nil {
		r := protocol.Failed(err)
		r.Change = "changed"
		return r
	}
	return protocol.Success("changed")
}

// readTimeZone is the node's time zone: the name in the target of the link
// /etc/localtime, below the time zone database, which is what programs use.
// Without /etc/localtime, programs use UTC, as timedatectl says. The name is
// empty when /etc/localtime is a copy of a zone's file instead of a link to
// it. timedatectl's own answer is not used: after a
// set-timezone that could not write the link ("Access denied" from SELinux
// on a node whose files were not labeled), timedated reports the new name
// while the node still has the old one, and a second try took that for done.
func (run Runtime) readTimeZone(ctx context.Context) (string, error) {
	target, err := os.Readlink(localTimeLink)
	if errors.Is(err, fs.ErrNotExist) {
		return "UTC", nil
	}
	if errors.Is(err, syscall.EINVAL) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read the link %s: %w", localTimeLink, err)
	}
	if _, name, found := strings.Cut(target, "zoneinfo/"); found {
		return name, nil
	}
	return "", nil
}

// TimeZone is the node's time zone, a name from the time zone database.
type TimeZone struct {
	Name string `yaml:"name" json:"name"`
}

// timeZoneName is a name from the time zone database, such as Europe/Berlin,
// America/Argentina/Buenos_Aires, UTC, or Etc/GMT+5.
var timeZoneName = regexp.MustCompile(`^[A-Za-z0-9_+-]+(/[A-Za-z0-9_+-]+)*$`)

var timeZoneRule = parsed(func(s string) error {
	if !timeZoneName.MatchString(s) {
		return errors.New("time_zone.name must be a name from the time zone database, such as Europe/Berlin or UTC: letters, digits, '_', '-', and '+', in parts separated by /")
	}
	return nil
})

func ValidateTimeZone(z TimeZone) error {
	if z.Name == "" {
		return invalid("name", "time_zone.name is required, such as Europe/Berlin or UTC")
	}
	return nil
}

// timeZoneDescription is what mmx describe-module shows for time_zone.
var timeZoneDescription = Description{
	Name: "time_zone", Topic: TopicHostNameTimeZoneLocaleAndAlternatives, Schema: reflect.TypeOf(TimeZone{}),
	Summary: "the node's time zone, set through timedatectl; running services read it when they start",
	Fields: []Field{
		{Name: "name", Required: true, Notes: "a name from the time zone database, such as Europe/Berlin or UTC; a name the node's /usr/share/zoneinfo does not have fails before any change"},
	},
	Example: "- time_zone:\n    name: Europe/Berlin\n",
	Results: []string{"status", "change", "value.time_zone_before", "value.time_zone_after"},
	Dryrun:  "supported; reads the time zone and reports the one that would be set; an unknown name fails",
}

// Prepare checks the inputs when the project loads.
func (z *TimeZone) Prepare(base string) error {
	return ValidateTimeZone(*z)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (z *TimeZone) Arguments(arguments *ArgumentList) {
	arguments.Public("name", &z.Name, timeZoneRule)
}

// Run does the module's work on the node.
func (z *TimeZone) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.TimeZone(ctx, *z)
}
