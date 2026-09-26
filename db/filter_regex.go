package db

import (
	"regexp"
	"strings"
	"sync"
)

// regexCache compila cada patrón distinto una sola vez (punto 6).
// Clave: el patrón una vez aplicadas las $options (por ejemplo "(?i)^a").
var regexCache sync.Map // string → *regexp.Regexp | centinela de error

var errRegexCache = errBadRegex{}

type errBadRegex struct{}

func (errBadRegex) Error() string { return "db: invalid regex" }

func compileCached(pattern string) (*regexp.Regexp, error) {
	if v, ok := regexCache.Load(pattern); ok {
		if _, bad := v.(errBadRegex); bad {
			return nil, ErrBadFilter
		}
		return v.(*regexp.Regexp), nil
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		regexCache.Store(pattern, errRegexCache)
		return nil, ErrBadFilter
	}
	regexCache.Store(pattern, re)
	return re, nil
}

// applyRegex trata $regex + $options juntos (las opciones pueden aparecer como hermanas).
// Se llama desde matchOps cuando $regex está presente; $options por si solo no hace nada.
func applyRegex(val any, exists bool, ops Document) (bool, error) {
	if !exists {
		return false, nil
	}
	s, ok := val.(string)
	if !ok {
		// un valor que no es string nunca coincide con $regex (estilo Mongo)
		return false, nil
	}
	pattern, _ := ops["$regex"].(string)
	options, _ := ops["$options"].(string)
	if strings.Contains(options, "i") && !strings.HasPrefix(pattern, "(?i)") {
		pattern = "(?i)" + pattern
	}
	re, err := compileCached(pattern)
	if err != nil {
		return false, err
	}
	return re.MatchString(s), nil
}
