package modules

import (
	"path/filepath"
	"slices"
	"strings"
	"unicode"
)

// ifupdownFile is what MMX reads of one file in ifupdown's format
// (interfaces(5)): enough to tell which interfaces a change touches, never
// a model of their settings.
type ifupdownFile struct {
	interfaces map[string]*ifupdownInterface
	// order lists the interface names in the order of their first line.
	order []string
	// fileLevelLines are the source, source-directory, and rename lines.
	fileLevelLines    []string
	sourcePatterns    []string
	sourceDirectories []string
	hasMapping        bool
}

// ifupdownInterface is what one file says about one interface. Each line is
// normalized to its words joined by one space.
type ifupdownInterface struct {
	// stanzaLines are the header and option lines of its iface stanzas.
	stanzaLines []string
	// bases are the interfaces that its stanzas inherit from.
	bases []string
	// classes are auto and the allow- classes that list it; flags are
	// no-auto-down and no-scripts.
	classes, flags []string
}

// ifupdownLogicalLines joins a line that ends in a backslash with the next
// one and skips blank lines and comments, as ifupdown does: a comment is a
// line whose first character that is not blank is #.
func ifupdownLogicalLines(text string) []string {
	var lines []string
	physical := strings.Split(text, "\n")
	for i := 0; i < len(physical); i++ {
		line := strings.TrimLeftFunc(physical[i], unicode.IsSpace)
		if strings.HasPrefix(line, "#") {
			continue
		}
		for strings.HasSuffix(line, "\\") && i+1 < len(physical) {
			i++
			line = strings.TrimSuffix(line, "\\") + physical[i]
		}
		if words := strings.Fields(strings.TrimSuffix(line, "\\")); len(words) > 0 {
			lines = append(lines, strings.Join(words, " "))
		}
	}
	return lines
}

func readIfupdownFile(text string) ifupdownFile {
	file := ifupdownFile{interfaces: map[string]*ifupdownInterface{}}
	named := func(name string) *ifupdownInterface {
		if file.interfaces[name] == nil {
			file.interfaces[name] = &ifupdownInterface{}
			file.order = append(file.order, name)
		}
		return file.interfaces[name]
	}
	addOnce := func(list *[]string, value string) {
		if !slices.Contains(*list, value) {
			*list = append(*list, value)
			slices.Sort(*list)
		}
	}
	// The stanza that option lines belong to; ifupdown ends it at every
	// other kind of line.
	var current *ifupdownInterface
	for _, line := range ifupdownLogicalLines(text) {
		words := strings.Fields(line)
		keyword := words[0]
		switch {
		case keyword == "iface" && len(words) > 1:
			current = named(words[1])
			current.stanzaLines = append(current.stanzaLines, line)
			for i := 2; i+1 < len(words); i++ {
				if words[i] == "inherits" {
					current.bases = append(current.bases, words[i+1])
				}
			}
			continue
		case keyword == "mapping":
			file.hasMapping = true
		case keyword == "auto" || strings.HasPrefix(keyword, "allow-"):
			class := keyword
			if class == "allow-auto" {
				class = "auto"
			}
			for _, name := range words[1:] {
				addOnce(&named(name).classes, class)
			}
		case keyword == "no-auto-down" || keyword == "no-scripts":
			for _, name := range words[1:] {
				addOnce(&named(name).flags, keyword)
			}
		case keyword == "source" || keyword == "source-directory" || keyword == "rename":
			file.fileLevelLines = append(file.fileLevelLines, line)
			if keyword == "source" {
				file.sourcePatterns = append(file.sourcePatterns, words[1:]...)
			} else if keyword == "source-directory" {
				file.sourceDirectories = append(file.sourceDirectories, words[1:]...)
			}
		default:
			if current != nil {
				current.stanzaLines = append(current.stanzaLines, line)
			}
			continue
		}
		current = nil
	}
	return file
}

// reads says whether this main file's source or source-directory lines read
// path. It does not follow sources inside sourced files.
func (f ifupdownFile) reads(path string) bool {
	absolute := func(name string) string {
		if filepath.IsAbs(name) {
			return filepath.Clean(name)
		}
		return filepath.Join(filepath.Dir(ifupdownMainFile), name)
	}
	for _, pattern := range f.sourcePatterns {
		if matched, err := filepath.Match(absolute(pattern), path); err == nil && matched {
			return true
		}
	}
	for _, directory := range f.sourceDirectories {
		if absolute(directory) == filepath.Dir(path) {
			return true
		}
	}
	return false
}

func (f ifupdownFile) declares(name string) bool {
	return f.interfaces[name] != nil && len(f.interfaces[name].stanzaLines) > 0
}

// key is everything this file says about an interface: its stanzas, the
// stanzas they inherit from in this file, its classes, and its flags. An
// interface changes when its key differs between the two files.
func (f ifupdownFile) key(name string) string {
	if f.interfaces[name] == nil {
		return ""
	}
	var key strings.Builder
	written := map[string]bool{}
	var writeStanzas func(string)
	writeStanzas = func(name string) {
		if written[name] || f.interfaces[name] == nil {
			return
		}
		written[name] = true
		for _, line := range f.interfaces[name].stanzaLines {
			key.WriteString(line + "\n")
		}
		for _, base := range f.interfaces[name].bases {
			writeStanzas(base)
		}
	}
	writeStanzas(name)
	key.WriteString("classes: " + strings.Join(f.interfaces[name].classes, " ") + "\n")
	key.WriteString("flags: " + strings.Join(f.interfaces[name].flags, " ") + "\n")
	return key.String()
}

// bringUpClass is the class that ifup -a brings the interface up with at
// boot: auto, then allow-hotplug; "" for neither.
func (f ifupdownFile) bringUpClass(name string) string {
	for _, class := range []string{"auto", "allow-hotplug"} {
		//nolint:nilaway // The caller requires file.declares(name), which checks that the interface map entry is non-nil.
		if slices.Contains(f.interfaces[name].classes, class) {
			return class
		}
	}
	return ""
}

// Only interface names and their class survive into the activation and
// restore processes, never stanza text or commands: ifdown and ifup read
// the saved files themselves.
type ifupdownRestoreData struct {
	previousExisted                   bool
	previousContentsSHA256            string
	TakeDownWithPreviousFile          []string                     `json:"interfaces_to_take_down_with_previous_file"`
	BringUpWithProposedFile           []ifupdownInterfaceToBringUp `json:"interfaces_to_bring_up_with_proposed_file"`
	TakeDownWithProposedFileOnRestore []string                     `json:"interfaces_to_take_down_with_proposed_file_on_restore"`
	BringUpWithPreviousFileOnRestore  []ifupdownInterfaceToBringUp `json:"interfaces_to_bring_up_with_previous_file_on_restore"`
}

type ifupdownInterfaceToBringUp struct {
	Interface string `json:"interface"`
	// ListedIn is auto or allow-hotplug.
	ListedIn string `json:"listed_in"`
}

func (d ifupdownRestoreData) names() []string {
	names := slices.Concat(d.TakeDownWithPreviousFile, d.TakeDownWithProposedFileOnRestore)
	for _, up := range slices.Concat(d.BringUpWithProposedFile, d.BringUpWithPreviousFileOnRestore) {
		names = append(names, up.Interface)
	}
	return names
}

// ifupdownInterfacesThatChange compares the interfaces of the previous and
// the proposed file. lo is left alone, as networking.service leaves it.
func ifupdownInterfacesThatChange(previous, proposed ifupdownFile) ifupdownRestoreData {
	data := ifupdownRestoreData{TakeDownWithPreviousFile: []string{}, BringUpWithProposedFile: []ifupdownInterfaceToBringUp{},
		TakeDownWithProposedFileOnRestore: []string{}, BringUpWithPreviousFileOnRestore: []ifupdownInterfaceToBringUp{}}
	changed := func(file ifupdownFile, name string) bool {
		return name != "lo" && file.declares(name) && previous.key(name) != proposed.key(name)
	}
	plan := func(file ifupdownFile, takeDown *[]string, bringUp *[]ifupdownInterfaceToBringUp) {
		for _, name := range file.order {
			if !changed(file, name) {
				continue
			}
			*takeDown = slices.Insert(*takeDown, 0, name)
			if class := file.bringUpClass(name); class != "" {
				*bringUp = append(*bringUp, ifupdownInterfaceToBringUp{name, class})
			}
		}
	}
	plan(previous, &data.TakeDownWithPreviousFile, &data.BringUpWithPreviousFileOnRestore)
	plan(proposed, &data.TakeDownWithProposedFileOnRestore, &data.BringUpWithProposedFile)
	return data
}

// ifupdownInterfaceName accepts the names that ifup and ifdown read as one
// interface: a Linux device name of at most 15 bytes, or an alias such as
// eth0:1. Without a mapping, ifupdown's interface names are device names.
func ifupdownInterfaceName(name string) bool {
	return name != "" && name != "." && name != ".." && len(name) <= 15 && !strings.HasPrefix(name, "-") &&
		!strings.ContainsAny(name, "/=") && !strings.ContainsFunc(name, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}
