// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"strconv"
	"strings"
	"unicode/utf16"

	"ewsgo/internal/core"
)

var rtfCP1252 = []rune("€\x00‚ƒ„…†‡ˆ‰Š‹Œ\x00Ž\x00\x00‘’“”•–—˜™š›œ\x00žŸ")

type rtfTextGroup struct {
	skip, fontTable bool
	uc, font        int
}

type importRTFBody struct {
	plain string
	rich  bool
}

// readImportRTF extracts the text of MS-OXRTFEX fromtext and bounded native
// RTF documents. For the latter the caller must retain the original RTF.
func readImportRTF(raw []byte) (importRTFBody, error) {
	if len(raw) > maxMessageImportBytes {
		return importRTFBody{}, core.ErrLimit
	}
	if !bytes.HasPrefix(raw, []byte("{\\rtf1")) {
		return importRTFBody{}, core.ErrInvalid
	}
	groups := []rtfTextGroup{{uc: 1}}
	fontCodepages := make(map[int]bool)
	var result strings.Builder
	var fromText bool
	var high rune
	var fallback int
	emit := func(ch rune) error {
		if fallback > 0 {
			fallback--
			return nil
		}
		if high != 0 || ch == 0 || ch < ' ' && ch != '\t' {
			return core.ErrInvalid
		}
		result.WriteRune(ch)
		return nil
	}
	for i := 1; i < len(raw); {
		group := &groups[len(groups)-1]
		ch := raw[i]
		i++
		switch ch {
		case '\r', '\n':
			continue
		case '{':
			if fallback != 0 || high != 0 || len(groups) == 64 {
				return importRTFBody{}, core.ErrInvalid
			}
			groups = append(groups, *group)
		case '}':
			if fallback != 0 || high != 0 {
				return importRTFBody{}, core.ErrInvalid
			}
			groups = groups[:len(groups)-1]
			if len(groups) == 0 {
				if len(bytes.Trim(raw[i:], "\r\n")) != 0 {
					return importRTFBody{}, core.ErrInvalid
				}
				return importRTFBody{plain: result.String(), rich: !fromText}, nil
			}
		case '\\':
			if i == len(raw) {
				return importRTFBody{}, core.ErrInvalid
			}
			if !rtfASCIILetter(raw[i]) {
				symbol := raw[i]
				i++
				if symbol == '*' {
					if len(groups) == 1 || fallback != 0 {
						return importRTFBody{}, core.ErrInvalid
					}
					group.skip = true
					continue
				}
				decoded, consumed, err := rtfTextSymbol(symbol, raw[i:])
				if err != nil {
					return importRTFBody{}, err
				}
				i += consumed
				if !group.skip {
					if symbol == '\'' && decoded >= 0x80 && fontCodepages[group.font] {
						return importRTFBody{}, core.ErrUnsupported
					}
					if err := emit(decoded); err != nil {
						return importRTFBody{}, err
					}
				}
				continue
			}
			word, number, hasNumber, consumed, err := rtfTextControl(raw[i:])
			if err != nil {
				return importRTFBody{}, err
			}
			i += consumed
			switch word {
			case "bin", "fromhtml", "htmltag", "mhtmltag", "object", "pict":
				return importRTFBody{}, core.ErrUnsupported
			}
			if group.skip {
				if group.fontTable && hasNumber {
					switch word {
					case "f":
						group.font = number
					case "fcharset":
						fontCodepages[group.font] = number != 0 && number != 1
					case "cpg":
						fontCodepages[group.font] = number != 1252
					}
				}
				continue
			}
			if fallback != 0 {
				return importRTFBody{}, core.ErrInvalid
			}
			switch word {
			case "rtf":
				if number != 1 || len(groups) != 1 {
					return importRTFBody{}, core.ErrInvalid
				}
			case "fromtext":
				if len(groups) != 1 || fromText || result.Len() != 0 || hasNumber {
					return importRTFBody{}, core.ErrInvalid
				}
				fromText = true
			case "fonttbl", "colortbl", "stylesheet", "info", "generator":
				if len(groups) == 1 {
					return importRTFBody{}, core.ErrInvalid
				}
				group.skip = true
				group.fontTable = word == "fonttbl"
			case "deff", "f":
				if !hasNumber {
					return importRTFBody{}, core.ErrInvalid
				}
				group.font = number
			case "ansicpg":
				if number != 1252 {
					return importRTFBody{}, core.ErrUnsupported
				}
			case "uc":
				if !hasNumber || number < 0 || number > 16 {
					return importRTFBody{}, core.ErrInvalid
				}
				group.uc = number
			case "u":
				if !hasNumber || number < -32768 || number > 32767 {
					return importRTFBody{}, core.ErrInvalid
				}
				value := rune(uint16(int16(number)))
				if value >= 0xD800 && value <= 0xDBFF && high == 0 {
					high = value
				} else {
					if utf16.IsSurrogate(value) || high != 0 {
						value = utf16.DecodeRune(high, value)
						high = 0
						if value == '\uFFFD' {
							return importRTFBody{}, core.ErrInvalid
						}
					}
					if err := emit(value); err != nil {
						return importRTFBody{}, err
					}
				}
				fallback = group.uc
			case "par", "line":
				if high != 0 {
					return importRTFBody{}, core.ErrInvalid
				}
				result.WriteString("\r\n")
			case "tab", "emdash", "endash", "bullet", "lquote", "rquote", "ldblquote", "rdblquote":
				literal := map[string]rune{"tab": '\t', "emdash": '—', "endash": '–', "bullet": '•',
					"lquote": '‘', "rquote": '’', "ldblquote": '“', "rdblquote": '”'}[word]
				if err := emit(literal); err != nil {
					return importRTFBody{}, err
				}
			case "ansi", "deflang", "deflangfe", "adeflang", "deftab", "viewkind",
				"pard", "plain", "ltrpar", "rtlpar", "ltrch", "rtlch", "ql", "qr", "qc", "qj",
				"af", "fs", "afs", "b", "i", "ul", "ulnone", "strike", "cf", "highlight",
				"lang", "langfe", "alang", "li", "ri", "fi", "sa", "sb", "sl", "slmult", "tx",
				"loch", "hich", "dbch", "nouicompat", "widctlpar", "nowidctlpar":
				// Presentation controls do not contribute to the encapsulated text.
			default:
				return importRTFBody{}, core.ErrUnsupported
			}
		default:
			if !group.skip {
				if ch >= 0x80 && fontCodepages[group.font] {
					return importRTFBody{}, core.ErrUnsupported
				}
				if err := emit(rtfTextByte(ch)); err != nil {
					return importRTFBody{}, err
				}
			}
		}
		if result.Len() > maxMessageImportBytes {
			return importRTFBody{}, core.ErrLimit
		}
	}
	return importRTFBody{}, core.ErrInvalid
}

func rtfASCIILetter(b byte) bool { return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' }

func rtfTextControl(raw []byte) (word string, number int, hasNumber bool, consumed int, err error) {
	for consumed < len(raw) && rtfASCIILetter(raw[consumed]) {
		consumed++
	}
	if consumed > 32 {
		return "", 0, false, 0, core.ErrInvalid
	}
	word = string(raw[:consumed])
	start := consumed
	if consumed < len(raw) && raw[consumed] == '-' {
		consumed++
	}
	digits := consumed
	for consumed < len(raw) && raw[consumed] >= '0' && raw[consumed] <= '9' {
		consumed++
	}
	if consumed != start {
		if digits == consumed || consumed-digits > 10 {
			return "", 0, false, 0, core.ErrInvalid
		}
		n, parseErr := strconv.ParseInt(string(raw[start:consumed]), 10, 32)
		if parseErr != nil {
			return "", 0, false, 0, core.ErrInvalid
		}
		number, hasNumber = int(n), true
	}
	if consumed < len(raw) && raw[consumed] == ' ' {
		consumed++
	}
	return word, number, hasNumber, consumed, nil
}

func rtfTextSymbol(symbol byte, tail []byte) (rune, int, error) {
	switch symbol {
	case '{', '}', '\\':
		return rune(symbol), 0, nil
	case '~':
		return '\u00A0', 0, nil
	case '-':
		return '\u00AD', 0, nil
	case '_':
		return '\u2011', 0, nil
	case '\'':
		if len(tail) >= 2 {
			value, err := strconv.ParseUint(string(tail[:2]), 16, 8)
			if err == nil {
				return rtfTextByte(byte(value)), 2, nil
			}
		}
		return 0, 0, core.ErrInvalid
	default:
		return 0, 0, core.ErrUnsupported
	}
}

func rtfTextByte(ch byte) rune {
	if ch >= 0x80 && ch < 0xa0 {
		return rtfCP1252[ch-0x80]
	}
	return rune(ch)
}
