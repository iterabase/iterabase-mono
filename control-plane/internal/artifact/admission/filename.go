package admission

import (
	"strings"
	"unicode/utf8"
)

// executableExtensions are dotted segments that make a multi-extension name an
// executable double-extension trick ("invoice.pdf.exe", "x.exe.pdf"). "com" is
// deliberately absent: as an inner segment it is overwhelmingly a domain name
// ("acme.com.pdf"), and as a final extension it is already unsupported.
var executableExtensions = map[string]bool{
	"exe": true, "scr": true, "bat": true, "cmd": true, "js": true,
	"vbs": true, "ps1": true, "msi": true, "dll": true, "jar": true,
	"sh": true, "app": true, "lnk": true, "hta": true,
}

// NormalizeFilename returns the display form of a claimed filename. ok is
// false when the name must be rejected as unsafe_filename: invalid UTF-8, a
// bidi override/isolate, an executable double extension, or nothing left
// after normalisation.
func NormalizeFilename(name string) (string, bool) {
	normalized, ok := normalizeFilename(name)
	return normalized, ok && normalized != ""
}

// normalizeFilename is NormalizeFilename that reports an empty result as ok, so
// Evaluate can accept a missing name for a generated (pasted) artifact.
func normalizeFilename(name string) (string, bool) {
	if !utf8.ValidString(name) {
		return "", false
	}
	if strings.ContainsFunc(name, isBidiControl) {
		return "", false
	}
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if isControl(r) {
			return -1
		}
		return r
	}, name)
	name = strings.Trim(name, " .")
	if hasExecutableDoubleExtension(name) {
		return "", false
	}
	return truncateFilename(name), true
}

func isBidiControl(r rune) bool {
	return (r >= 0x202A && r <= 0x202E) || (r >= 0x2066 && r <= 0x2069) || r == 0x200E || r == 0x200F
}

func isControl(r rune) bool {
	return r < 0x20 || (r >= 0x7F && r <= 0x9F)
}

func hasExecutableDoubleExtension(name string) bool {
	segments := strings.Split(name, ".")
	if len(segments) < 3 {
		return false
	}
	for _, segment := range segments[1:] {
		if executableExtensions[strings.ToLower(strings.TrimSpace(segment))] {
			return true
		}
	}
	return false
}

// truncateFilename bounds name to MaxFilenameBytes, truncating the stem at a
// rune boundary and keeping a short extension.
func truncateFilename(name string) string {
	if len(name) <= MaxFilenameBytes {
		return name
	}
	ext := ""
	if i := strings.LastIndexByte(name, '.'); i > 0 && len(name)-i <= 16 {
		ext = name[i:]
		name = name[:i]
	}
	limit := MaxFilenameBytes - len(ext)
	for limit > 0 && !utf8.RuneStart(name[limit]) {
		limit--
	}
	return strings.TrimRight(name[:limit], " .") + ext
}

// extensionOf returns the lowercased final extension without the dot.
func extensionOf(name string) string {
	i := strings.LastIndexByte(name, '.')
	if i < 0 {
		return ""
	}
	return strings.ToLower(name[i+1:])
}
