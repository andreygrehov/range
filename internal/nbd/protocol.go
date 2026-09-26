package nbd

import ()

const (
	requestMagic     = 0x25609513
	simpleReplyMagic = 0x67446698
	optionMagic      = 0x49484156454F5054 // IHAVEOPT
	optionReplyMagic = 0x3E889045565A9

	cmdRead  = 0
	cmdWrite = 1
	cmdDisc  = 2
	cmdFlush = 3
	cmdTrim  = 4

	optExportName = 1
	optAbort      = 2
	optList       = 3
	optGo         = 7

	repAck      = 1
	repInfo     = 3
	repErrUnsup = 0x80000001

	flagFixedNewstyle = 1
	flagNoZeroes      = 2
	flagHasFlags      = 1
	flagReadOnly      = 2

	errPerm  = 1
	errIO    = 5
	errInval = 22

	maxRequest = 32 << 20
	maxWorkers = 16
)
