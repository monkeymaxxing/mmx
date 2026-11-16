/*
Copyright 2014 Alexander Okoli

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package goutils

import (
	"bytes"
	"fmt"
	"unicode"
)

/*
Abbreviate abbreviates a string using ellipses. This will turn  the string "Now is the time for all good men" into "Now is the time for..."

Specifically, the algorithm is as follows:

    - If str is less than maxWidth characters long, return it.
    - Else abbreviate it to (str[0:maxWidth - 3] + "...").
    - If maxWidth is less than 4, return an illegal argument error.
    - In no case will it return a string of length greater than maxWidth.

Parameters:
    str -  the string to check
    maxWidth - maximum length of result string, must be at least 4

Returns:
    string - abbreviated string
    error - if the width is too small
*/
func Abbreviate(str string, maxWidth int) (string, error) {
	return AbbreviateFull(str, 0, maxWidth)
}

/*
AbbreviateFull abbreviates a string using ellipses. This will turn the string "Now is the time for all good men" into "...is the time for..."
This function works like Abbreviate(string, int), but allows you to specify a "left edge" offset. Note that this left edge is not
necessarily going to be the leftmost character in the result, or the first character following the ellipses, but it will appear
somewhere in the result.
In no case will it return a string of length greater than maxWidth.

Parameters:
    str - the string to check
    offset - left edge of source string
    maxWidth - maximum length of result string, must be at least 4

Returns:
    string - abbreviated string
    error - if the width is too small
*/
func AbbreviateFull(str string, offset int, maxWidth int) (string, error) {
	if str == "" {
		return "", nil
	}
	if maxWidth < 4 {
		err := fmt.Errorf("stringutils illegal argument: Minimum abbreviation width is 4")
		return "", err
	}
	if len(str) <= maxWidth {
		return str, nil
	}
	if offset > len(str) {
		offset = len(str)
	}
	if len(str)-offset < (maxWidth - 3) { // 15 - 5 < 10 - 3 =  10 < 7
		offset = len(str) - (maxWidth - 3)
	}
	abrevMarker := "..."
	if offset <= 4 {
		return str[0:maxWidth-3] + abrevMarker, nil // str.substring(0, maxWidth - 3) + abrevMarker;
	}
	if maxWidth < 7 {
		err := fmt.Errorf("stringutils illegal argument: Minimum abbreviation width with offset is 7")
		return "", err
	}
	if (offset + maxWidth - 3) < len(str) { // 5 + (10-3) < 15 = 12 < 15
		abrevStr, _ := Abbreviate(str[offset:len(str)], (maxWidth - 3))
		return abrevMarker + abrevStr, nil // abrevMarker + abbreviate(str.substring(offset), maxWidth - 3);
	}
	return abrevMarker + str[(len(str)-(maxWidth-3)):len(str)], nil // abrevMarker + str.substring(str.length() - (maxWidth - 3));
}

/*
DeleteWhiteSpace deletes all whitespaces from a string as defined by unicode.IsSpace(rune).
It returns the string without whitespaces.

Parameter:
    str - the string to delete whitespace from, may be nil

Returns:
    the string without whitespaces
*/
func DeleteWhiteSpace(str string) string {
	if str == "" {
		return str
	}
	sz := len(str)
	var chs bytes.Buffer
	count := 0
	for i := 0; i < sz; i++ {
		ch := rune(str[i])
		if !unicode.IsSpace(ch) {
			chs.WriteRune(ch)
			count++
		}
	}
	if count == sz {
		return str
	}
	return chs.String()
}

