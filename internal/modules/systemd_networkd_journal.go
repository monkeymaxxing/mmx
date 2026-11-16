package modules

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"mmx/internal/protocol"
)

// CONFIG_FILE, when present, is authoritative. MESSAGE is used only for
// entries without it. A period is a boundary only at the end of a sentence,
// not at the start of .d/, .bak, or another longer filename.
func networkdMessageNamesFile(message, path string) bool {
	for offset := 0; offset < len(message); {
		index := strings.Index(message[offset:], path)
		if index < 0 {
			return false
		}
		start := offset + index
		end := start + len(path)
		before := start == 0 || strings.ContainsRune(" \t\r\n\"'", rune(message[start-1]))
		after := end == len(message)
		if !after {
			after = strings.ContainsRune(": \t\r\n\"'", rune(message[end]))
			if message[end] == '.' {
				after = end+1 == len(message) || strings.ContainsRune(" \t\r\n\"'", rune(message[end+1]))
			}
		}
		if before && after {
			return true
		}
		offset = end
	}
	return false
}

type networkJournalEntry struct {
	Priority                   int
	Message, ConfigurationFile string
	HasConfigurationFile       bool
}

func checkNetworkdJournal(destination, cursor, text string) (string, error) {
	entries, err := decodeNetworkJournal(cursor, text)
	if err != nil {
		return "", err
	}
	var diagnostics []string
	for _, entry := range entries {
		namesFile := entry.ConfigurationFile == destination
		if !entry.HasConfigurationFile {
			namesFile = networkdMessageNamesFile(entry.Message, destination)
		}
		if entry.Priority <= 4 && namesFile {
			diagnostics = append(diagnostics, entry.Message)
		}
	}
	return strings.Join(diagnostics, "; "), nil
}

func networkJournalPosition(ctx context.Context, run Runtime) (string, error) {
	output := run.RunCommand(ctx, []string{"journalctl", "--no-pager", "-n", "1", "-o", "cat", "--show-cursor"}, "")
	if err := commandFailure(output, "Reading the journal position before the network change"); err != nil {
		return "", err
	}
	if !strings.Contains(output.StandardOutput, protocol.TruncationMarker) {
		for _, line := range strings.Split(output.StandardOutput, "\n") {
			if cursor, ok := strings.CutPrefix(line, "-- cursor: "); ok && cursor != "" {
				return cursor, nil
			}
		}
	}
	return "", fmt.Errorf("The journal did not provide a position before the network change.")
}
func readNetworkServiceJournal(ctx context.Context, run Runtime, service, cursor string) ([]networkJournalEntry, error) {
	if cursor == "" {
		return nil, fmt.Errorf("The journal position before the network change is missing.")
	}
	output := run.RunCommand(ctx, []string{"journalctl", "--sync"}, "")
	if err := commandFailure(output, "Synchronizing the journal after the network change"); err != nil {
		return nil, err
	}
	output = run.RunCommand(ctx, []string{"journalctl", "--no-pager", "--cursor", cursor, "-u", service, "-n", "200", "-o", "json", "--output-fields=__CURSOR,PRIORITY,MESSAGE,CONFIG_FILE,CONFIG_LINE,SYSLOG_IDENTIFIER,_SYSTEMD_UNIT"}, "")
	if err := commandFailure(output, "Reading the journal after the network change"); err != nil {
		return nil, err
	}
	return decodeNetworkJournal(cursor, output.StandardOutput)
}

func decodeNetworkJournal(cursor, text string) ([]networkJournalEntry, error) {
	if strings.Contains(text, protocol.TruncationMarker) {
		return nil, fmt.Errorf("The systemd journal output after the network change exceeded the output limit; MMX could not check whether systemd-networkd accepted the network configuration file.")
	}
	lines := strings.Split(strings.TrimSpace(text), "\n")
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	// At the command's 200-entry limit, more entries may have been omitted.
	// Refuse because this read cannot establish that it contains every warning.
	if len(lines) >= 200 {
		return nil, fmt.Errorf("The systemd journal output after the network change reached 200 entries; MMX could not check whether systemd-networkd accepted the network configuration file.")
	}
	var entries []networkJournalEntry
	for _, line := range lines {
		var entry map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &entry); err != nil || entry == nil {
			return nil, fmt.Errorf("a systemd journal entry after the network change could not be decoded.")
		}
		var position string
		if json.Unmarshal(entry["__CURSOR"], &position) != nil || position == "" {
			return nil, fmt.Errorf("a systemd journal entry after the network change has no readable __CURSOR field.")
		}
		// --cursor may include the anchor when that entry belongs to this service.
		// If it belongs to another service, retain the first returned entry.
		if position == cursor {
			continue
		}
		var priority, message, configurationFile string
		if json.Unmarshal(entry["PRIORITY"], &priority) != nil || json.Unmarshal(entry["MESSAGE"], &message) != nil || string(entry["PRIORITY"]) == "null" || string(entry["MESSAGE"]) == "null" {
			return nil, fmt.Errorf("a systemd journal entry after the network change has no readable PRIORITY or MESSAGE field.")
		}
		level, err := strconv.Atoi(priority)
		if err != nil || level < 0 || level > 7 {
			return nil, fmt.Errorf("a systemd journal entry after the network change has an invalid PRIORITY field.")
		}
		file, structured := entry["CONFIG_FILE"]
		if structured && (json.Unmarshal(file, &configurationFile) != nil || string(file) == "null") {
			return nil, fmt.Errorf("a systemd journal entry after the network change has no readable CONFIG_FILE path.")
		}
		if number, exists := entry["CONFIG_LINE"]; exists {
			var lineNumber string
			if json.Unmarshal(number, &lineNumber) != nil || string(number) == "null" {
				return nil, fmt.Errorf("a systemd journal entry after the network change has no readable CONFIG_LINE number.")
			}
		}
		entries = append(entries, networkJournalEntry{level, message, configurationFile, structured})
	}
	return entries, nil
}
