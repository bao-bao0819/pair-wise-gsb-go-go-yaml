package parser

// Option represents parser's option.
type Option func(p *parser)

// AllowDuplicateMapKey allow the use of keys with the same name in the same map,
// but by default, this is not permitted.
func AllowDuplicateMapKey() Option {
	return func(p *parser) {
		p.allowDuplicateMapKey = true
	}
}

// UniqueMapKeys enables strict mapping key uniqueness validation.
//
// Instead of comparing key text literally, keys are judged equivalent under
// the current scalar resolution rules and explicit tags: "0x10" and "16" are
// equal integer keys, while the string "1" and the integer 1 are not. See the
// documentation of UniqueMapKeyError for the full set of rules.
//
// When enabled, the default literal duplicate key check is replaced by this
// semantic check.
func UniqueMapKeys() Option {
	return func(p *parser) {
		p.uniqueMapKeys = true
	}
}
