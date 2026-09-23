package db

import (
	"regexp"
	"strings"
	"sync"
)

// regexCache compiles each distinct pattern once (point 6).
// Key: pattern after $options applied (e.g. "(?i)^a").
var regexCache sync.Map // string → *regexp.Regexp | error sentinel

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

// applyRegex handles $regex + $options together (options may appear as sibling).
// Call from matchOps when $regex is present; $options alone is a no-op.
func applyRegex(val any, exists bool, ops Document) (bool, error) {
	if !exists {
		return false, nil
	}
	s, ok := val.(string)
	if !ok {
		// non-string never matches $regex (Mongo-like)
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
