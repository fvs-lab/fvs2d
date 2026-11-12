//go:build cgo

package main

/*
#include <stdlib.h>
*/
import "C"
import "encoding/json"

func encodeResponse(value any) *C.char {
	data, _ := json.Marshal(value)
	return C.CString(string(data))
}
