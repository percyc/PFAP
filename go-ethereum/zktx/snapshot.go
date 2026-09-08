package zktx

/*
#include "smtcgo.hpp"
#include <stdlib.h>
*/
import "C"

import (
	"github.com/ethereum/go-ethereum/common"
	"unsafe"
)

// SMTSnapshot is independent of the execution/mining singleton. Its owner must
// serialize access, never mutate a published snapshot, and call Close on eviction.
type SMTSnapshot struct{ handle unsafe.Pointer }

func NewSMTSnapshot() *SMTSnapshot         { return &SMTSnapshot{C.smtSnapshotNew()} }
func (s *SMTSnapshot) Clone() *SMTSnapshot { return &SMTSnapshot{C.smtSnapshotClone(s.handle)} }
func (s *SMTSnapshot) Close() {
	if s.handle != nil {
		C.smtSnapshotDelete(s.handle)
		s.handle = nil
	}
}
func (s *SMTSnapshot) Insert(cmt common.Hash) {
	c := C.CString(cmt.Hex())
	defer C.free(unsafe.Pointer(c))
	C.smtSnapshotInsert(s.handle, c)
}
func (s *SMTSnapshot) Root() common.Hash {
	c := C.smtSnapshotRoot(s.handle)
	defer C.smtFree(c)
	return common.HexToHash(C.GoString(c))
}
func (s *SMTSnapshot) Witness(cmt common.Hash) string {
	c := C.CString(cmt.Hex())
	defer C.free(unsafe.Pointer(c))
	p := C.smtSnapshotProve(s.handle, c)
	defer C.smtFree(p)
	return C.GoString(p)
}
