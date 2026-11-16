package moduleio

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"strings"
)

const help = `module-json request [--inputs NAME,...]
  Validate a module request and write the unchanged request to stdout.
  With --inputs, reject input names outside the comma-separated list.
module-json get [--type string|integer|number|boolean|array|object|null|json] POINTER
  Read a module request and write the value selected by a JSON Pointer,
  such as /inputs/path or /mode.
  String output is exact text without a newline; NUL is rejected. Other types
  emit JSON; integer requires a whole signed 64-bit number and prints decimal.
  A missing value causes an error. An explicit null is not a missing value:
  --type null and --type json accept it.
module-json result --mode check|apply --change CHANGE [--status STATUS] [--secret]
  [--reason TEXT] [--output NAME TEXT]... [--output-json NAME JSON]...
  [--fingerprint TEXT] [--reason-stdin | --diff-stdin | --output-stdin NAME]
  Validate a module result and write the result as JSON. The status defaults
  to succeeded. Use --reason-stdin, --diff-stdin, or --output-stdin NAME
  to read text from stdin. Only one stdin option is allowed per call.
  Pass long text, differences, and secrets through stdin. Use --reason only
  for fixed public messages.
`

// CommandError reports invalid helper command arguments. Request/value validation
// errors instead mean the helper performed its check and found invalid data.
type CommandError struct{ error }

// Run is shared by both binaries and runs without loading a project or node state.
func Run(args []string, input io.Reader, output io.Writer) error {
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		_, err := io.WriteString(output, help)
		return err
	}
	fs := flag.NewFlagSet("module-json "+args[0], flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	switch args[0] {
	case "request":
		names := fs.String("inputs", "", "allowed input names")
		if err := parseFlags(fs, args[1:], 0); err != nil {
			return helperHelp(err, output)
		}
		data, err := readBounded(input, MaxRequestBytes)
		if err != nil {
			return err
		}
		request, err := ReadRequest(bytes.NewReader(data))
		if err != nil {
			return err
		}
		if *names != "" {
			allowed := strings.Split(*names, ",")
			for _, name := range allowed {
				if name == "" {
					return &CommandError{fmt.Errorf("--inputs must not contain an empty input name")}
				}
			}
			if err := fields(request.Inputs, allowed...); err != nil {
				return fmt.Errorf("module request contains an input name not listed in --inputs")
			}
		}
		// Keep the original encoding so escaping cannot inflate a valid request
		// beyond the limit when the script passes it to subsequent get calls.
		_, err = output.Write(data)
		return err
	case "get":
		kind := fs.String("type", "json", "expected value type")
		if err := parseFlags(fs, args[1:], 1); err != nil {
			return helperHelp(err, output)
		}
		switch *kind {
		case "string", "integer", "number", "boolean", "array", "object", "null", "json":
		default:
			return &CommandError{fmt.Errorf("module-json get --type must be string, integer, number, boolean, array, object, null, or json")}
		}
		request, err := ReadRequest(input)
		if err != nil {
			return err
		}
		root := map[string]any{"protocol": json.Number("1"), "mode": request.Mode, "inputs": request.Inputs}
		v, err := lookup(root, fs.Arg(0))
		if err != nil {
			return err
		}
		return writeValue(output, v, *kind)
	case "result":
		return writeHelperResult(args[1:], input, output)
	default:
		return &CommandError{fmt.Errorf("unknown module-json command; see module-json --help")}
	}
}

func parseFlags(fs *flag.FlagSet, args []string, want int) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return &CommandError{fmt.Errorf("invalid module-json flags; see module-json --help")}
	}
	if fs.NArg() != want {
		return &CommandError{fmt.Errorf("unexpected module-json arguments; see module-json --help")}
	}
	return nil
}

func helperHelp(err error, output io.Writer) error {
	if !errors.Is(err, flag.ErrHelp) {
		return err
	}
	_, err = io.WriteString(output, help)
	return err
}

func lookup(root any, pointer string) (any, error) {
	if pointer == "" {
		return root, nil
	}
	if !strings.HasPrefix(pointer, "/") {
		return nil, &CommandError{fmt.Errorf("a nonempty JSON Pointer must start with /")}
	}
	for _, part := range strings.Split(pointer[1:], "/") {
		var key strings.Builder
		for i := 0; i < len(part); i++ {
			if part[i] != '~' {
				key.WriteByte(part[i])
				continue
			}
			i++
			if i == len(part) {
				return nil, &CommandError{fmt.Errorf("invalid JSON Pointer escape: use ~0 for ~ or ~1 for /")}
			}
			switch part[i] {
			case '0':
				key.WriteByte('~')
			case '1':
				key.WriteByte('/')
			default:
				return nil, &CommandError{fmt.Errorf("invalid JSON Pointer escape: use ~0 for ~ or ~1 for /")}
			}
		}
		name := key.String()
		switch v := root.(type) {
		case map[string]any:
			var exists bool
			root, exists = v[name]
			if !exists {
				return nil, fmt.Errorf("missing value at requested JSON Pointer")
			}
		case []any:
			if name == "" || (len(name) > 1 && name[0] == '0') || strings.IndexFunc(name, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
				return nil, &CommandError{fmt.Errorf("the JSON Pointer array index must contain only digits, with no leading zero unless the index is 0")}
			}
			index, err := strconv.Atoi(name)
			if err != nil || index >= len(v) {
				return nil, fmt.Errorf("missing value at requested JSON Pointer")
			}
			root = v[index]
		default:
			return nil, fmt.Errorf("the JSON Pointer cannot select a child of a string, number, boolean, or null value")
		}
	}
	return root, nil
}

func writeValue(output io.Writer, v any, kind string) error {
	actual := "null"
	switch v.(type) {
	case string:
		actual = "string"
	case bool:
		actual = "boolean"
	case json.Number:
		actual = "number"
	case []any:
		actual = "array"
	case map[string]any:
		actual = "object"
	}
	if kind == "integer" {
		number, ok := v.(json.Number)
		if !ok {
			return fmt.Errorf("module-json get --type integer requires an integer, found %s", actual)
		}
		integer, err := decimalInteger(number.String())
		if err != nil {
			return err
		}
		_, err = io.WriteString(output, strconv.FormatInt(integer, 10))
		return err
	}
	if kind != "json" && kind != actual {
		return fmt.Errorf("module-json get expected %s, found %s", kind, actual)
	}
	if kind == "string" {
		text := v.(string)
		if strings.ContainsRune(text, 0) {
			return fmt.Errorf("string output contains a NUL byte, which Bash cannot store in a variable; use --type json or a language other than Bash")
		}
		_, err := io.WriteString(output, text)
		return err
	}
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("cannot encode the selected value as JSON")
	}
	_, err = output.Write(data)
	return err
}

// decimalInteger converts an already validated JSON number without floating-point
// rounding or allocating exponent-sized buffers.
func decimalInteger(number string) (int64, error) {
	invalid := fmt.Errorf("expected a whole number in the signed 64-bit integer range")
	negative := strings.HasPrefix(number, "-")
	number = strings.TrimPrefix(number, "-")
	mantissa, exponentText, hasExponent := strings.Cut(strings.ToLower(number), "e")
	whole, fraction, _ := strings.Cut(mantissa, ".")
	digits := strings.TrimLeft(whole+fraction, "0")
	if digits == "" {
		return 0, nil
	}
	exponent := int64(0)
	if hasExponent {
		var err error
		exponent, err = strconv.ParseInt(exponentText, 10, 32)
		if err != nil {
			return 0, invalid
		}
	}
	trimmed := strings.TrimRight(digits, "0")
	shift := exponent - int64(len(fraction)) + int64(len(digits)-len(trimmed))
	if shift < 0 || int64(len(trimmed))+shift > 19 {
		return 0, invalid
	}
	decimal := trimmed + strings.Repeat("0", int(shift))
	if negative {
		decimal = "-" + decimal
	}
	integer, err := strconv.ParseInt(decimal, 10, 64)
	if err != nil {
		return 0, invalid
	}
	return integer, nil
}

// writeHelperResult encodes the result that module-json result's arguments
// describe. An output takes two arguments, its name and its value, so the
// arguments are read here rather than by the flag package.
func writeHelperResult(args []string, input io.Reader, output io.Writer) error {
	result := Result{Protocol: Version, Status: "succeeded"}
	mode, stdinField, stdinOutput := "", "", ""
	outputs := map[string]any{}
	bad := func(format string, values ...any) error {
		return &CommandError{fmt.Errorf(format+"; see module-json --help", values...)}
	}
	value := func(i int, option string) (string, error) {
		if i >= len(args) {
			return "", bad("%s requires a value", option)
		}
		return args[i], nil
	}
	for i := 0; i < len(args); i++ {
		option := args[i]
		switch option {
		case "--help", "-h":
			_, err := io.WriteString(output, help)
			return err
		case "--mode", "--status", "--change", "--reason", "--fingerprint":
			i++
			text, err := value(i, option)
			if err != nil {
				return err
			}
			switch option {
			case "--mode":
				mode = text
			case "--status":
				result.Status = text
			case "--change":
				result.Change = text
			case "--reason":
				result.Reason = text
			case "--fingerprint":
				result.Fingerprint = text
			}
		case "--secret":
			result.Secret = true
		case "--output", "--output-json":
			if i+2 >= len(args) {
				return bad("%s requires NAME and a value", option)
			}
			name, text := args[i+1], args[i+2]
			i += 2
			if _, exists := outputs[name]; exists {
				return bad("the output %s is given twice", name)
			}
			outputs[name] = text
			if option == "--output-json" {
				v, err := parseJSON([]byte(text))
				if err != nil {
					return fmt.Errorf("invalid JSON for the output %s", name)
				}
				outputs[name] = v
			}
		case "--reason-stdin", "--diff-stdin", "--output-stdin":
			if stdinField != "" {
				return bad("module-json result accepts only one of --reason-stdin, --diff-stdin, or --output-stdin per call")
			}
			stdinField = option
			if option == "--output-stdin" {
				i++
				name, err := value(i, option)
				if err != nil {
					return err
				}
				stdinOutput = name
			}
		default:
			return bad("unknown result argument at position %d", i+1)
		}
	}
	if stdinField == "--reason-stdin" && result.Reason != "" {
		return bad("--reason-stdin cannot be combined with a nonempty --reason")
	}
	check := result
	if stdinField == "--reason-stdin" {
		check.Reason = "reason supplied on stdin"
	}
	if err := check.validate(mode); err != nil {
		return &CommandError{err}
	}
	if stdinField != "" {
		data, err := readBounded(input, MaxResultBytes)
		if err != nil {
			return err
		}
		switch stdinField {
		case "--reason-stdin":
			result.Reason = string(data)
		case "--diff-stdin":
			result.Diff = string(data)
		case "--output-stdin":
			if _, exists := outputs[stdinOutput]; exists {
				return bad("the output %s is given twice", stdinOutput)
			}
			outputs[stdinOutput] = string(data)
		}
	}
	if len(outputs) > 0 {
		result.Outputs, _ = json.Marshal(outputs)
	}
	return WriteResult(output, mode, result)
}
