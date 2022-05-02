package easyrpc

import (
	"fmt"
	"reflect"
	"testing"
)

type Calc int

type Args struct{ Num1, Num2 int }

func (c Calc) Plus(args Args, reply *int) error {
	*reply = args.Num1 + args.Num2
	return nil
}

func _assert(condition bool, msg string, v ...interface{}) {
	if !condition {
		panic(fmt.Sprintf("assertion failed: "+msg, v...))
	}
}

func TestNewService(t *testing.T) {
	var c Calc
	s := newService(&c)
	_assert(len(s.method) == 1, "wrong service Method, expect 1, but got %d", len(s.method))
	mType := s.method["Plus"]
	_assert(mType != nil, "wrong Method, Plus shouldn't nil")
}

func TestMethodType_Call(t *testing.T) {
	var c Calc
	s := newService(&c)
	mType := s.method["Plus"]

	argv := mType.newArgv()
	replyv := mType.newReplyv()
	argv.Set(reflect.ValueOf(Args{1, 2}))
	err := s.call(mType, argv, replyv)
	_assert(err == nil && *replyv.Interface().(*int) == 3 && mType.NumCalls() == 1, "failed to call Clac.Plus")
}
