package myplugin

import (
	"api"
	"fmt"
)

//gojvm:extends api/Plugin
//gojvm:implements api/Listener
type Hello struct {
	Count int
	Name  string
}

func (h *Hello) OnEnable() {
	h.Log("enabled " + h.Name)
}

func (h *Hello) OnCommand(label string, argc int32) {
	h.Count += int(argc)
	h.Log(fmt.Sprintf("cmd %s argc=%d total=%d", label, argc, h.Count))
}

func (h *Hello) OnEvent(name string) {
	h.Log("event " + name)
}

func Boot() *Hello {
	h := &Hello{Name: "demo"}
	h.OnEvent("boot")
	return h
}
