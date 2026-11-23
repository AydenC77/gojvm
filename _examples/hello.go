package main

import (
	"fmt"
	"java/lang"
	"java/util"
)

const Greeting = "Hello from Go on the JVM!"

var counter = 0

func fib(n int) int {
	if n < 2 {
		return n
	}
	return fib(n-1) + fib(n-2)
}

func isPrime(n int) bool {
	if n < 2 {
		return false
	}
	for i := 2; i*i <= n; i++ {
		if n%i == 0 {
			return false
		}
	}
	return true
}

func main() {
	fmt.Println(Greeting)
	fmt.Println("fib(20) =", fib(20))

	sum := 0
	for i := range 10 {
		sum += i
	}
	fmt.Println("sum 0..9 =", sum)

	primes := util.NewArrayList()
	for n := 1; n < 30; n++ {
		if isPrime(n) {
			primes.Add(n)
		}
	}
	fmt.Println("primes:", primes, "size:", primes.Size())

	sb := lang.NewStringBuilder()
	sb.Append("x=").Append(3.5).Append(' ').Append(true)
	fmt.Println(sb.ToString())

	lang.System.Out.Println("via System.out")
	fmt.Printf("%d squared is %d, pi ~ %.3f\n", 7, 7*7, 3.14159)

	var f float64 = 10
	f /= 4
	fmt.Println(f, f > 2, "a" < "b", counter)
	counter++
	counter += 41
	fmt.Println(counter)

	switch counter {
	case 1, 2:
		fmt.Println("small")
	case 42:
		fmt.Println("the answer")
	default:
		fmt.Println("other")
	}
	m := util.NewHashMap()
	m.Put("one", 1)
	m.Put("two", 2)
	v := m.Get("two").(int)
	fmt.Println("two =", v+1)
}
