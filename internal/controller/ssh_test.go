package controller

import "testing"

func TestSudoAsksForPasswordInTheWordsOfSudoAndSudoRs(t *testing.T) {
	for message, asks := range map[string]bool{
		"sudo: a password is required": true,
		"sudo: a terminal is required to read the password; either use the -S option to read from standard input or configure an askpass helper": true,
		"sudo: interactive authentication is required":          true,
		"mmx-test is not in the sudoers file.":                  false,
		"sudo: Sorry, user mmx-test may not run sudo on node1.": false,
	} {
		if got := sudoAsksForPassword(message); got != asks {
			t.Errorf("sudoAsksForPassword(%q) = %v, want %v", message, got, asks)
		}
	}
}
