// Copyright (c) 2022 RethinkDNS and its authors.
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.
package dnsx

import "errors"

const (
	// Start: Transaction started
	Start = iota
	// Complete : Transaction completed successfully
	Complete
	// SendFailed : Failed to send query
	SendFailed
	// NoResponse : Got no response
	NoResponse
	// BadQuery : Malformed input
	BadQuery
	// BadResponse : Response was invalid
	BadResponse
	// InternalError : This should never happen
	InternalError
	// TransportError: Transport has issues
	TransportError
)

var noerr = errors.New("no underlying error")

type QueryError struct {
	status int
	err    error
}

func (e *QueryError) Error() string {
	return e.err.Error()
}

func (e *QueryError) Unwrap() error {
	return e.err
}

func (e *QueryError) Status() int {
	return e.status
}

func (e *QueryError) SendFailed() bool {
	return e.status == SendFailed
}

func newQueryError(no int, err error) *QueryError {
	if err == nil {
		err = noerr
	}
	return &QueryError{no, err}
}

func NewSendFailedQueryError(err error) *QueryError {
	return newQueryError(SendFailed, err)
}

func NewNoResponseQueryError(err error) *QueryError {
	return newQueryError(NoResponse, err)
}

func NewInternalQueryError(err error) *QueryError {
	return newQueryError(InternalError, err)
}

func NewBadQueryError(err error) *QueryError {
	return newQueryError(BadQuery, err)
}

func NewBadResponseQueryError(err error) *QueryError {
	return newQueryError(BadResponse, err)
}

func NewTransportQueryError(err error) *QueryError {
	return newQueryError(TransportError, err)
}
