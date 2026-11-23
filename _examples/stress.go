package main

import (
	"fmt"
	"java/lang"
	"java/util"
	"math"
)

const (
	A = iota
	B
	C
)

func classify(n int) string {
	switch {
	case n < 0:
		return "neg"
	case n == 0:
		return "zero"
	}
	return "pos"
}

func both(a, b bool) bool { return a && b || !a && !b }

func collatz(n int) int {
	steps := 0
	for n != 1 {
		if n%2 == 0 {
			n /= 2
		} else {
			n = 3*n + 1
		}
		steps++
	}
	return steps
}

func main() {
	fmt.Println(A, B, C, classify(-5), classify(0), classify(9))
	fmt.Println(both(true, true), both(true, false), both(false, false))
	fmt.Println("collatz 27:", collatz(27))

outer:
	for i := 0; i < 5; i++ {
		for j := 0; j < 5; j++ {
			if j == 3 {
				continue outer
			}
			if i == 3 {
				break outer
			}
			fmt.Print(i*10+j, " ")
		}
	}
	fmt.Println()

	x := 7
	ok := x > 3 && x < 10
	fmt.Println("ok:", ok, "str:", "n="+fmt.Sprint(x)+"!")
	var i8 int8 = 127
	i8++
	fmt.Println("wrap:", i8, int32(300)*2, float64(x)/2, int(3.99+0.01))
	fmt.Println(math.Sqrt(2), math.Pow(2, 10), math.Max(3, 4.5), math.Pi)

	arr := lang.NewString("hello").ToCharArray()
	n := 0
	for _, ch := range arr {
		if ch == 'l' {
			n++
		}
	}
	fmt.Println("l count:", n, "len:", len(arr))

	list := util.NewArrayList()
	for i := 0; i < 5; i++ {
		list.Add(i * i)
	}
	util.Collections.Reverse(list)
	total := 0
	for i := 0; i < int(list.Size()); i++ {
		total += list.Get(int32(i)).(int)
	}
	fmt.Println(list, total, lang.Math.Abs(-7.5), lang.Integer.ParseInt("123")+1)
	fmt.Println(lang.String.ValueOf(true), lang.Integer.MAX_VALUE, lang.Long.MAX_VALUE)
}
