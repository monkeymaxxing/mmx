package value

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// Secret keeps sensitive data separate from its printable and durable form.
// Only the transport encoder may include Data. A named reference can be
// refreshed on retry; a computed value cannot be recreated by replaying work.
type Secret struct {
	origin    *Secret // the scope secret a delivered value replaces (Within)
	load      func() (*Secret, error)
	Data      any
	Name      string
	Path      []string
	Available bool
	// FromNode says where a secret that started on a node came from. Saved
	// history keeps it in place of the value, so a resume can read a
	// read_file secret again or name the task whose output it was.
	FromNode *NodeOrigin
	// HiddenBecause says why the node task whose result held this value was
	// hidden (Trace.HiddenBecause), so that an error about the value can name
	// the node task that read the secret. It is public text.
	HiddenBecause string
}

// NodeOrigin is where a secret that started on a node came from: the fleet
// stage and node, the node task when one produced it, and for read_file the
// file it read. All of it is public.
type NodeOrigin struct {
	FleetStage   string `json:"fleet_stage"`
	Node         string `json:"node"`
	NodeTask     string `json:"node_task,omitempty"`
	ReadFilePath string `json:"read_file_path,omitempty"`
}

func Sensitive(v any) *Secret                  { return &Secret{Data: v, Available: true} }
func (s *Secret) String() string               { return "[redacted]" }
func (s *Secret) MarshalJSON() ([]byte, error) { return json.Marshal(s.record(false)) }
func (s *Secret) record(private bool) map[string]any {
	fields := map[string]any{}
	if s.Name != "" {
		fields["name"] = s.Name
	}
	if len(s.Path) > 0 {
		fields["path"] = s.Path
	}
	if s.FromNode != nil {
		fields["from_node"] = s.FromNode
	}
	if private && s.Available {
		fields["data"] = Transport(s.Data)
	}
	return map[string]any{"$mmx_secret": fields}
}

// Transport is used only for authenticated messages and private volatile
// handoff files. Normal json.Marshal always suppresses the values.
func Transport(v any) any {
	switch x := v.(type) {
	case *Secret:
		return x.record(true)
	case map[string]any:
		if x == nil {
			return x
		}
		out := map[string]any{}
		for k, v := range x {
			out[k] = Transport(v)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = Transport(v)
		}
		return out
	default:
		return v
	}
}

// Restore reads the internal records in saved or transported runtime values.
// The module runner rejects reserved keys in results, so a module cannot forge
// a record.
func Restore(v any) any {
	switch x := v.(type) {
	case map[string]any:
		if x == nil {
			return x
		}
		if text, ok := x["$mmx_truncated"].(string); ok && len(x) == 1 {
			return Truncated{text}
		}
		if b, ok := restoreBinary(x); ok {
			return b
		}
		if fields, ok := x["$mmx_secret"].(map[string]any); ok && len(x) == 1 {
			s := &Secret{}
			s.Name, _ = fields["name"].(string)
			if path, ok := fields["path"].([]any); ok {
				for _, p := range path {
					if p, ok := p.(string); ok {
						s.Path = append(s.Path, p)
					}
				}
			}
			if origin, ok := fields["from_node"].(map[string]any); ok {
				s.FromNode = &NodeOrigin{}
				s.FromNode.FleetStage, _ = origin["fleet_stage"].(string)
				s.FromNode.Node, _ = origin["node"].(string)
				s.FromNode.NodeTask, _ = origin["node_task"].(string)
				s.FromNode.ReadFilePath, _ = origin["read_file_path"].(string)
			}
			s.Data, s.Available = fields["data"]
			s.Data = Restore(s.Data)
			return s
		}
		out := map[string]any{}
		for k, v := range x {
			out[k] = Restore(v)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = Restore(v)
		}
		return out
	default:
		return v
	}
}

// HasReservedKey reports an internal record's mapping key anywhere in v.
func HasReservedKey(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		for k, child := range x {
			if k == "$mmx_secret" || k == "$mmx_truncated" || k == "$mmx_binary" || HasReservedKey(child) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if HasReservedKey(child) {
				return true
			}
		}
	}
	return false
}

// HiddenBecause is the HiddenBecause of the first secret in v that has one.
func HiddenBecause(v any) string {
	switch x := v.(type) {
	case *Secret:
		return x.HiddenBecause
	case map[string]any:
		for _, k := range slices.Sorted(maps.Keys(x)) {
			if because := HiddenBecause(x[k]); because != "" {
				return because
			}
		}
	case []any:
		for _, item := range x {
			if because := HiddenBecause(item); because != "" {
				return because
			}
		}
	}
	return ""
}

func HasSecrets(v any) bool {
	switch x := v.(type) {
	case *Secret:
		return true
	case Scope:
		return HasSecrets(map[string]any(x))
	case map[string]any:
		for _, v := range x {
			if HasSecrets(v) {
				return true
			}
		}
	case []any:
		for _, v := range x {
			if HasSecrets(v) {
				return true
			}
		}
	}
	return false
}

// Plain is deliberately explicit at module and template boundaries. Derived
// results retain their sensitivity even when the value's representation changes.
func Plain(v any) any {
	switch x := v.(type) {
	case Truncated:
		return x.Text
	case *Secret:
		return Plain(x.Data) //nolint:nilaway // The type switch dereferences only allocated *Secret values; missing lookups return nil interfaces.
	case map[string]any:
		out := map[string]any{}
		for k, v := range x {
			out[k] = Plain(v)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = Plain(v)
		}
		return out
	default:
		return v
	}
}

// Trace records one task's reads of secrets. Used hides the task's result:
// the task, or a task inside it, read a secret, so Used spreads to every
// trace around. hidesTasksStartedInside makes every node task that starts
// inside the task from then on start with Used: it is set when the task's
// own fields read a secret (a when, a loop's items), and on every trace
// around a node task that read a secret and may have left it on the node
// (HidesLaterTasks), where a later node task could read it back without
// reading a secret value. A secret read by set_vars, show, or assert stays
// in values, which keep their secrecy, so it hides only what reads them.
type Trace struct {
	Used bool
	// read: the task's own fields read a secret, not only a task inside it.
	read                    bool
	hidesTasksStartedInside bool
	// derived is a trace for one value inside its parent's task (TraceReads):
	// its reads are its parent's own.
	derived bool
	// Task is the node task's name, which the engine sets.
	Task string
	// hiddenBecause names, for the node tasks that start hidden inside the
	// task, the node task that read the secret.
	hiddenBecause string
	parent        *Trace
	// loginsHandedOnlyToATool are the texts that must not show in what the
	// task reports (LoginHandedOnlyToATool).
	loginsHandedOnlyToATool []string
	secretTexts             []string
}

const traceKey = "\x00secret_trace"

// Trace copies s for one task. The copy shares s's vars, results, and
// availability marks, so a mark made inside the task (a task block phase's
// skipped result) reaches the scope its caller reads.
func (s Scope) Trace() (Scope, *Trace) {
	if s.Missing() == nil {
		s[""] = map[string]string{}
	}
	out := Scope{}
	for k, v := range s {
		out[k] = v
	}
	parent, _ := s[traceKey].(*Trace)
	hidden := parent != nil && parent.hidesTasksStartedInside
	trace := &Trace{parent: parent, Used: hidden, hidesTasksStartedInside: hidden}
	if hidden {
		trace.hiddenBecause = parent.hiddenBecause
	}
	out[traceKey] = trace
	return out, trace
}

// HidesLaterTasks is called when a node task that may have left a secret
// it read on the node ends: every node task that starts afterwards inside
// the role calls and node task blocks around it is hidden.
func (t *Trace) HidesLaterTasks() {
	if !t.read {
		return
	}
	for around := t.parent; around != nil; around = around.parent {
		around.hidesTasksStartedInside = true
		if around.hiddenBecause == "" {
			around.hiddenBecause = fmt.Sprintf("node task %s read a secret before it started and may have left it on the node; a node task that starts before %s is not hidden", t.Task, t.Task)
		}
	}
}

// HiddenBecause says why the task's result is hidden, or "" when it is not.
func (t *Trace) HiddenBecause() string {
	switch {
	case !t.Used:
		return ""
	case t.read:
		return "it read a secret"
	case t.hiddenBecause != "":
		return t.hiddenBecause
	}
	return "a node task inside it read a secret"
}

// TraceReads is Trace for one derived value: it starts unused, so the value is
// sensitive only when it reads a secret, and each read still marks s's traces.
func (s Scope) TraceReads() (Scope, *Trace) {
	out, trace := s.Trace()
	trace.Used, trace.hidesTasksStartedInside, trace.derived = false, false, true
	return out, trace
}
func (s Scope) touch(v any) {
	if !HasSecrets(v) {
		return
	}
	texts := secretValueTexts(v, false)
	t, _ := s[traceKey].(*Trace)
	own := true
	for ; t != nil; t = t.parent {
		t.Used = true
		if own {
			t.read, t.hidesTasksStartedInside = true, true
			if t.hiddenBecause == "" && t.Task != "" {
				t.hiddenBecause = "node task " + t.Task + " read a secret before it started"
			}
			own = t.derived
		}
		for _, text := range texts {
			if !slices.Contains(t.secretTexts, text) {
				t.secretTexts = append(t.secretTexts, text)
			}
		}
	}
}

// EnvironmentText is how MMX passes a resolved value to a command's env:
// lists and mappings as JSON, and scalar values as their ordinary text.
func EnvironmentText(v any) (string, error) {
	switch plain := Plain(v).(type) {
	case []any, map[string]any:
		b, err := json.Marshal(plain)
		return string(b), err
	}
	return Scalar(v)
}

// SecretValueTexts collects secret values without reading references or marking
// a trace. It includes the forms MMX creates when formatting environments and
// shortening diagnostics. Public siblings of secret values are ignored.
func SecretValueTexts(v any) []string { return secretValueTexts(v, false) }

func secretTextForms(text string) []string {
	var forms []string
	if text != "" {
		forms = append(forms, text)
	}
	// Keep the exact value and its trimmed form, then each trimmed nonempty
	// line: MMX may trim diagnostics or keep only their last lines.
	for _, form := range append([]string{text}, strings.Split(text, "\n")...) {
		if trimmed := strings.TrimSpace(form); trimmed != "" && !slices.Contains(forms, trimmed) {
			forms = append(forms, trimmed)
		}
	}
	return forms
}

func secretValueTexts(v any, sensitive bool) []string {
	if x, ok := v.(*Secret); ok {
		return secretValueTexts(x.Data, true)
	}
	var texts []string
	if sensitive {
		if text, err := EnvironmentText(v); err == nil {
			texts = secretTextForms(text)
		}
	}
	switch x := v.(type) {
	case map[string]any:
		for key, child := range x {
			if sensitive {
				texts = append(texts, secretTextForms(key)...)
			}
			texts = append(texts, secretValueTexts(child, sensitive)...)
		}
	case []any:
		for _, child := range x {
			texts = append(texts, secretValueTexts(child, sensitive)...)
		}
	}
	return texts
}

// SecretTexts lists the secret values read in this task and its surrounding
// role or node task block. These bytes never belong in a saved result.
func (s Scope) SecretTexts() []string {
	trace, _ := s[traceKey].(*Trace)
	var texts []string
	for ; trace != nil; trace = trace.parent {
		texts = append(texts, trace.secretTexts...)
	}
	return texts
}

func (s Scope) Data() map[string]any {
	out := map[string]any{}
	for k, v := range s {
		if k != "" && !strings.HasPrefix(k, "\x00") {
			out[k] = v
		}
	}
	return out
}

// ResolveWithoutMarkingATrace resolves v as Resolve does, and marks no trace
// when v reads a secret. It is for a caller that only passes the value on
// and puts it into nothing that a task reports: a role call that hands a
// secret to an input of its role, and a module that hands a login only to
// its tool (LoginHandedOnlyToATool). The value keeps its secrecy, so whoever
// reads it from where the caller puts it marks the trace of its own task.
func (s Scope) ResolveWithoutMarkingATrace(v any) (any, error) {
	untraced := Scope{}
	for k, held := range s {
		if k != traceKey {
			untraced[k] = held
		}
	}
	return untraced.Resolve(v)
}

// LoginHandedOnlyToATool resolves written, the written value of an input
// that MMX hands only to a tool as a login: a user name, a password, or a
// key that a module gives to its tool through a private file or a local
// socket, and that the module puts nowhere else. It resolves as Resolve does,
// with one difference: the read marks no trace, so a secret in a login does
// not hide the task's result. The task's trace remembers the text of a login
// that holds a secret instead, and the node task execution engine replaces
// every occurrence of it in what the task reports
// (WithoutLoginsHandedOnlyToATool). A login without a secret, such as a user
// name written in the playbook, is not remembered.
func (s Scope) LoginHandedOnlyToATool(written any) (any, error) {
	login, err := s.ResolveWithoutMarkingATrace(written)
	if err != nil {
		return nil, err
	}
	if HasSecrets(login) {
		if text, err := Scalar(login); err == nil {
			s.FormOfLoginHandedOnlyToATool(text)
		}
	}
	return login, nil
}

// FormOfLoginHandedOnlyToATool remembers one more text that must not show in
// what the task reports: a form in which a module gives a login that holds a
// secret to its tool, or in which the tool keeps or sends it, such as the
// Base64 text of a user name and a password. An empty text is never
// remembered.
func (s Scope) FormOfLoginHandedOnlyToATool(form string) {
	trace, _ := s[traceKey].(*Trace)
	if trace == nil || form == "" {
		return
	}
	for _, known := range trace.loginsHandedOnlyToATool {
		if known == form {
			return
		}
	}
	trace.loginsHandedOnlyToATool = append(trace.loginsHandedOnlyToATool, form)
}

// LoginsHandedOnlyToATool lists the texts that the task of this scope
// remembered: each login that holds a secret, in each of its forms.
func (s Scope) LoginsHandedOnlyToATool() []string {
	trace, _ := s[traceKey].(*Trace)
	if trace == nil {
		return nil
	}
	return trace.loginsHandedOnlyToATool
}

// WithoutLoginsHandedOnlyToATool returns v with every occurrence of one of
// logins, in every text inside v, replaced by [redacted]. A secret inside v
// stays as it is: it never shows.
func WithoutLoginsHandedOnlyToATool(v any, logins []string) any {
	if len(logins) == 0 {
		return v
	}
	switch x := v.(type) {
	case string:
		return TextWithoutLoginsHandedOnlyToATool(x, logins)
	case Truncated:
		return Truncated{TextWithoutLoginsHandedOnlyToATool(x.Text, logins)}
	case Binary:
		return Binary{[]byte(TextWithoutLoginsHandedOnlyToATool(string(x.Bytes), logins))}
	case map[string]any:
		if x == nil {
			return x
		}
		out := map[string]any{}
		for k, child := range x {
			out[TextWithoutLoginsHandedOnlyToATool(k, logins)] = WithoutLoginsHandedOnlyToATool(child, logins)
		}
		return out
	case []any:
		if x == nil {
			return x
		}
		out := make([]any, len(x))
		for i, child := range x {
			out[i] = WithoutLoginsHandedOnlyToATool(child, logins)
		}
		return out
	default:
		return v
	}
}

// TextWithoutLoginsHandedOnlyToATool returns text with every occurrence of
// one of logins replaced by [redacted]. Occurrences that overlap or touch
// become one [redacted], so no part of a login is left where two of them
// share characters.
func TextWithoutLoginsHandedOnlyToATool(text string, logins []string) string {
	// covered[i] says that the byte at i belongs to an occurrence.
	var covered []bool
	for _, login := range logins {
		if login == "" {
			continue
		}
		for from := 0; ; {
			at := strings.Index(text[from:], login)
			if at < 0 {
				break
			}
			if covered == nil {
				covered = make([]bool, len(text))
			}
			for i := from + at; i < from+at+len(login); i++ {
				covered[i] = true
			}
			// The next occurrence may start inside this one.
			from += at + 1
		}
	}
	if covered == nil {
		return text
	}
	var out strings.Builder
	for i := 0; i < len(text); i++ {
		if !covered[i] {
			out.WriteByte(text[i])
		} else if i == 0 || !covered[i-1] {
			out.WriteString("[redacted]")
		}
	}
	return out.String()
}

// Refresh restores named values only. Missing computed values remain marked
// and fail when actually consumed, without replaying their completed producer.
func Refresh(v any, lookup func(string) (any, error)) (any, error) {
	switch x := v.(type) {
	case *Secret:
		if x.Name == "" || x.Available {
			return x, nil
		}
		raw, err := lookup(x.Name)
		if err != nil {
			return nil, err
		}
		current := raw
		for _, part := range x.Path {
			switch c := current.(type) {
			case map[string]any:
				var ok bool
				current, ok = c[part]
				if !ok {
					return nil, fmt.Errorf("secret %s no longer has the selected field", x.Name)
				}
			case []any:
				n, err := strconv.Atoi(part)
				if err != nil || n < 0 || n >= len(c) {
					return nil, fmt.Errorf("secret %s changed structure", x.Name)
				}
				current = c[n]
			default:
				return nil, fmt.Errorf("secret %s changed structure", x.Name)
			}
		}
		return &Secret{Data: current, Name: x.Name, Path: x.Path, Available: true}, nil
	case map[string]any:
		if x == nil {
			return x, nil
		}
		out := map[string]any{}
		for k, v := range x {
			next, err := Refresh(v, lookup)
			if err != nil {
				return nil, err
			}
			out[k] = next
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			next, err := Refresh(v, lookup)
			if err != nil {
				return nil, err
			}
			out[i] = next
		}
		return out, nil
	default:
		return v, nil
	}
}

// CollectSecrets records, for each scope secret a Within result delivers, the
// value to send in its place. Parts that several references select from one
// secret are united.
func CollectSecrets(v any, into map[*Secret]*Secret) {
	switch x := v.(type) {
	case *Secret:
		original := x
		if x.origin != nil {
			original = x.origin
		}
		if held, ok := into[original]; ok && held != x {
			united := *held
			united.Data = unite(held.Data, x.Data) //nolint:nilaway // The type switch dereferences only allocated *Secret values; missing lookups return nil interfaces.
			x = &united
		}
		into[original] = x
	case map[string]any:
		for _, v := range x {
			CollectSecrets(v, into)
		}
	case []any:
		for _, v := range x {
			CollectSecrets(v, into)
		}
	}
}

// unite merges two selections from the same data: maps by key, lists by
// position, where nil marks an unselected element.
func unite(a, b any) any {
	switch x := a.(type) {
	case nil:
		return b
	case map[string]any:
		if y, ok := b.(map[string]any); ok {
			out := map[string]any{}
			for k, v := range x {
				out[k] = v
			}
			for k, v := range y {
				out[k] = unite(out[k], v)
			}
			return out
		}
	case []any:
		if y, ok := b.([]any); ok && len(x) == len(y) {
			out := make([]any, len(x))
			for i := range x {
				out[i] = unite(x[i], y[i])
			}
			return out
		}
	}
	return a
}
func KeepSecrets(v any, allowed map[*Secret]*Secret) any {
	switch x := v.(type) {
	case *Secret:
		if delivered, ok := allowed[x]; ok {
			return delivered
		}
		return &Secret{Name: x.Name, Path: x.Path}
	case map[string]any:
		out := map[string]any{}
		for k, v := range x {
			out[k] = KeepSecrets(v, allowed)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = KeepSecrets(v, allowed)
		}
		return out
	default:
		return v
	}
}

// Completed outputs are resolved only if later work reads them. Merely listing
// or publishing an unused output must not unlock a vault.
func Deferred(v any, lookup func(string) (any, error), refresh bool) any {
	switch x := v.(type) {
	case *Secret:
		if (x.Available && !refresh) || x.Name == "" {
			return x
		}
		copy := *x
		copy.Available = false
		copy.Data = nil
		source := copy
		copy.load = func() (*Secret, error) {
			v, err := Refresh(&source, lookup)
			if err != nil {
				return nil, &SecretLoadError{err}
			}
			return v.(*Secret), nil
		}
		return &copy
	case map[string]any:
		if x == nil {
			return x
		}
		out := map[string]any{}
		for k, v := range x {
			out[k] = Deferred(v, lookup, refresh)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = Deferred(v, lookup, refresh)
		}
		return out
	default:
		return v
	}
}

type SecretLoadError struct{ Err error }

func (e *SecretLoadError) Error() string { return e.Err.Error() }
func (e *SecretLoadError) Unwrap() error { return e.Err }

// loadSecrets loads deferred secrets in v. For delivery, each loaded secret
// records the scope secret it replaces.
func loadSecrets(v any, delivery bool) (any, error) {
	switch x := v.(type) {
	case *Secret:
		//nolint:nilaway // The type switch dereferences only allocated *Secret values; missing lookups return nil interfaces.
		if x.load == nil {
			return x, nil
		}
		loaded, err := x.load()
		if err != nil {
			return nil, err
		}
		if delivery {
			loaded.origin = x
		}
		return loaded, nil
	case map[string]any:
		out := map[string]any{}
		for k, v := range x {
			loaded, err := loadSecrets(v, delivery)
			if err != nil {
				return nil, err
			}
			out[k] = loaded
		}
		return out, nil
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			loaded, err := loadSecrets(v, delivery)
			if err != nil {
				return nil, err
			}
			out[i] = loaded
		}
		return out, nil
	default:
		return v, nil
	}
}
func RequireAvailable(v any) error {
	switch x := v.(type) {
	case *Secret:
		//nolint:nilaway // The type switch dereferences only allocated *Secret values; missing lookups return nil interfaces.
		if !x.Available {
			return &UnavailableError{"secret output", x.unavailable()}
		}
		return RequireAvailable(x.Data)
	case map[string]any:
		for _, v := range x {
			if err := RequireAvailable(v); err != nil {
				return err
			}
		}
	case []any:
		for _, v := range x {
			if err := RequireAvailable(v); err != nil {
				return err
			}
		}
	}
	return nil
}

func HasPrivateData(v any) bool {
	switch x := v.(type) {
	case *Secret:
		return x.Available
	case map[string]any:
		for _, v := range x {
			if HasPrivateData(v) {
				return true
			}
		}
	case []any:
		for _, v := range x {
			if HasPrivateData(v) {
				return true
			}
		}
	}
	return false
}

func (s Scope) InheritTrace(parent Scope) { s[traceKey] = parent[traceKey] }

// Display keeps the ordinary terminal view free of internal recovery metadata.
func Display(v any) any {
	switch x := v.(type) {
	case Truncated:
		return x.Text
	case Binary:
		return fmt.Sprintf("[binary content, %d bytes]", len(x.Bytes))
	case *Secret:
		return "[redacted]"
	case map[string]any:
		out := map[string]any{}
		for k, v := range x {
			out[k] = Display(v)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = Display(v)
		}
		return out
	default:
		return v
	}
}

// unavailable says why a secret output that was not saved cannot be read now,
// naming where it came from and what produces it again.
func (s *Secret) unavailable() string {
	const again = "start a new mmx run of the playbook to produce the secret output again"
	//nolint:nilaway // Callers reach unavailable only with allocated secrets from constructors or successful deferred loads.
	switch o := s.FromNode; {
	case o == nil:
		return "secret output is unavailable; run the current playbook to produce the secret output again"
	case o.ReadFilePath != "":
		return fmt.Sprintf("secret output is unavailable: read_file node task %s of fleet stage %s read the secret output from %s on node %s, but MMX has not read the file again; %s", o.NodeTask, o.FleetStage, o.ReadFilePath, o.Node, again)
	case o.NodeTask != "":
		return fmt.Sprintf("secret output is unavailable: node task %s of fleet stage %s on node %s produced the secret output. MMX does not save secret values or rerun completed node tasks to recover secret output; %s", o.NodeTask, o.FleetStage, o.Node, again)
	default:
		return fmt.Sprintf("secret output is unavailable: fleet stage %s on node %s produced the secret output. MMX does not save secret values or rerun completed fleet stages to recover secret output; %s", o.FleetStage, o.Node, again)
	}
}

// FromNode returns v with origin recorded on each secret computed on the node
// that has no complete origin yet. A secret that read_file read keeps its path
// and task; named secrets keep their names, which a resume loads again.
func FromNode(v any, origin NodeOrigin) any {
	switch x := v.(type) {
	case *Secret:
		if x.Name != "" || (x.FromNode != nil && x.FromNode.FleetStage != "") {
			return x
		}
		copy := *x
		complete := origin
		if x.FromNode != nil {
			complete.ReadFilePath = x.FromNode.ReadFilePath
			if x.FromNode.NodeTask != "" {
				complete.NodeTask = x.FromNode.NodeTask //nolint:nilaway // The enclosing x.FromNode != nil check guards this origin field access.
			}
		}
		copy.FromNode = &complete
		return &copy
	case map[string]any:
		if x == nil {
			return x
		}
		out := map[string]any{}
		for k, v := range x {
			out[k] = FromNode(v, origin)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = FromNode(v, origin)
		}
		return out
	default:
		return v
	}
}

// ReadAgain makes each secret that read_file read on a node, which saved
// history holds only by its node and path, read that file again through read
// when later work consumes it. Work that never reads it contacts no node.
func ReadAgain(v any, read func(NodeOrigin) (any, error)) any {
	switch x := v.(type) {
	case *Secret:
		if x.Available || x.load != nil || x.FromNode == nil || x.FromNode.ReadFilePath == "" {
			return x
		}
		copy := *x
		origin := *x.FromNode
		copy.load = func() (*Secret, error) {
			data, err := read(origin)
			if err != nil {
				return nil, &SecretLoadError{err}
			}
			return &Secret{Data: data, Available: true, FromNode: &origin}, nil
		}
		return &copy
	case map[string]any:
		if x == nil {
			return x
		}
		out := map[string]any{}
		for k, v := range x {
			out[k] = ReadAgain(v, read)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = ReadAgain(v, read)
		}
		return out
	default:
		return v
	}
}
