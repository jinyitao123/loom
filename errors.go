package loom

import "errors"

var (
	ErrMaxIterations      = errors.New("loom: max iterations exceeded")
	ErrBudgetExhausted    = errors.New("loom: global step budget exhausted")
	ErrStepNotFound       = errors.New("loom: step not found")
	ErrCheckpointNotFound = errors.New("loom: checkpoint not found")
	ErrCorruptCheckpoint  = errors.New("loom: corrupt checkpoint")
	ErrNestedTx           = errors.New("loom: nested transactions not supported")
)
