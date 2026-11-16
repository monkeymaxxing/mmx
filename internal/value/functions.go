package value

import (
	"bytes"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"text/template"

	"github.com/Masterminds/sprig/v3"
	"go.yaml.in/yaml/v3"
)

// withheldSprigFunctions are the Sprig functions MMX does not offer: their
// result does not depend on their arguments alone. They read the clock or the
// machine's time zone, produce random values (a salt or a key included), or
// read the environment or the network. Without them the same template renders
// the same bytes on every run, and an unchanged rerun reports unchanged.
// MMX's copy of Sprig in third_party/ has none of them; this list names them
// for the error a template that calls one gets.
var withheldSprigFunctions = []string{
	// The clock and the time zone.
	"now", "ago", "date", "date_in_zone", "dateInZone", "date_modify", "dateModify", "must_date_modify", "mustDateModify",
	"htmlDate", "htmlDateInZone", "toDate", "mustToDate", "unixEpoch", "durationRound",
	// Random values.
	"randAlphaNum", "randAlpha", "randAscii", "randNumeric", "randBytes", "randInt", "shuffle", "uuidv4",
	"bcrypt", "htpasswd", "encryptAES", "genPrivateKey", "buildCustomCert", "genCA", "genCAWithKey",
	"genSelfSignedCert", "genSelfSignedCertWithKey", "genSignedCert", "genSignedCertWithKey",
	// The environment and the network.
	"env", "expandenv", "getHostByName",
	// A greeting, not a function of anything.
	"hello",
}

var templateFunctions = func() template.FuncMap {
	functions := sprig.TxtFuncMap()
	functions["toYaml"] = toYAML
	functions["percent"] = percent
	return functions
}()

// TemplateFunctions are the functions a file template may call: Sprig's, under
// Sprig's names and argument order, without the withheld ones, and MMX's own
// toYaml and percent.
func TemplateFunctions() template.FuncMap { return templateFunctions }

// toYAML renders a value as YAML with sorted keys and two-space indentation,
// without a final line end, as Helm's function of the same name does.
func toYAML(v any) (string, error) {
	var out bytes.Buffer
	encoder := yaml.NewEncoder(&out)
	encoder.SetIndent(2)
	if err := encoder.Encode(v); err != nil {
		return "", fmt.Errorf("toYaml: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return "", fmt.Errorf("toYaml: %w", err)
	}
	return strings.TrimSuffix(out.String(), "\n"), nil
}

var undefinedFunction = regexp.MustCompile(`function "([^"]+)" not defined`)

// explainWithheld says why a template that calls a withheld function fails.
func explainWithheld(err error) error {
	if m := undefinedFunction.FindStringSubmatch(err.Error()); m != nil && slices.Contains(withheldSprigFunctions, m[1]) {
		return fmt.Errorf("template function %s is not available: the function reads the clock, random values, the environment, or the network. MMX offers only functions whose result depends on their arguments alone, so the same template renders the same bytes on every run", m[1])
	}
	return err
}
