package jvm

import "testing"

func TestJDK(t *testing.T) {
	cp, err := NewClassPath("")
	if err != nil {
		t.Skip(err)
	}
	defer cp.Close()
	t.Log("jdk:", cp.Home)
	ms := cp.MethodsNamed("java/io/PrintStream", "println")
	if len(ms) < 5 {
		t.Fatalf("println overloads: %d", len(ms))
	}
	if !cp.IsSubclass("java/util/ArrayList", "java/util/List") {
		t.Fatal("ArrayList !<: List")
	}
	if !cp.HasPackage("java/util") {
		t.Fatal("no java/util")
	}
	f, _ := cp.FieldNamed("java/lang/System", "out")
	if f == nil {
		t.Fatal("no System.out")
	}
}
