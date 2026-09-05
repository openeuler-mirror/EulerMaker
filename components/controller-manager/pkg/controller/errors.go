package controller

import "errors"

type PermanentError interface {
	error
	Permanent() bool
}
type permanentError struct{ error }

func (permanentError) Permanent() bool { return true }
func NewPermanentError(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}
func IsPermanent(err error) bool {
	var target PermanentError
	return errors.As(err, &target) && target.Permanent()
}
