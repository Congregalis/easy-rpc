package main

import (
	"time"
)

type Calc int

type Args struct{ Num1, Num2 int }

func (c Calc) Plus(args Args, reply *int) error {
	*reply = args.Num1 + args.Num2
	return nil
}

func (c Calc) Sleep(args Args, reply *int) error {
	time.Sleep(time.Second * time.Duration(args.Num1))
	*reply = args.Num1 + args.Num2
	return nil
}
