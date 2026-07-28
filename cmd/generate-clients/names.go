package main

import (
	"strconv"
	"strings"
	"unicode"
)

var goInitialisms = map[string]string{
	"api":    "API",
	"cpu":    "CPU",
	"etag":   "ETag",
	"http":   "HTTP",
	"https":  "HTTPS",
	"id":     "ID",
	"ip":     "IP",
	"json":   "JSON",
	"pty":    "PTY",
	"sha":    "SHA",
	"sha256": "SHA256",
	"ttl":    "TTL",
	"uri":    "URI",
	"url":    "URL",
	"utf":    "UTF",
	"uuid":   "UUID",
}

func goExportedIdentifier(value string) string {
	words := identifierWords(value)
	var result strings.Builder
	for _, word := range words {
		lower := strings.ToLower(word)
		if initialism, ok := goInitialisms[lower]; ok {
			result.WriteString(initialism)
			continue
		}
		runes := []rune(lower)
		if len(runes) == 0 {
			continue
		}
		result.WriteRune(unicode.ToUpper(runes[0]))
		result.WriteString(string(runes[1:]))
	}
	if result.Len() == 0 {
		return "Value"
	}
	if first := []rune(result.String())[0]; unicode.IsDigit(first) {
		return "Value" + result.String()
	}
	return result.String()
}

func identifierWords(value string) []string {
	var words []string
	var current []rune
	runes := []rune(value)
	flush := func() {
		if len(current) > 0 {
			words = append(words, string(current))
			current = nil
		}
	}
	for index, candidate := range runes {
		if !unicode.IsLetter(candidate) && !unicode.IsDigit(candidate) {
			flush()
			continue
		}
		if len(current) > 0 && unicode.IsUpper(candidate) {
			previous := runes[index-1]
			var next rune
			if index+1 < len(runes) {
				next = runes[index+1]
			}
			if unicode.IsLower(previous) || unicode.IsDigit(previous) || (unicode.IsUpper(previous) && unicode.IsLower(next)) {
				flush()
			}
		}
		current = append(current, candidate)
	}
	flush()
	return words
}

func pythonString(value string) string {
	return strconv.Quote(value)
}

func typeScriptString(value string) string {
	return strconv.Quote(value)
}
