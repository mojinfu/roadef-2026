package main

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

var (
	pLoadEnv, pNewModel, pAddVars, pAddConstrs *syscall.LazyProc
	pSetIntAttr, pUpdate, pOptimize            *syscall.LazyProc
	pGetDblArr, pGetIntAttr                    *syscall.LazyProc
)

func load() {
	dll := syscall.NewLazyDLL(`D:\code\challenge-roadef-2026\.venv\Lib\site-packages\gurobipy\gurobi130.dll`)
	pLoadEnv = dll.NewProc("GRBloadenv")
	pNewModel = dll.NewProc("GRBnewmodel")
	pAddVars = dll.NewProc("GRBaddvars")
	pAddConstrs = dll.NewProc("GRBaddconstrs")
	pSetIntAttr = dll.NewProc("GRBsetintattr")
	pUpdate = dll.NewProc("GRBupdatemodel")
	pOptimize = dll.NewProc("GRBoptimize")
	pGetDblArr = dll.NewProc("GRBgetdblattrarray")
	pGetIntAttr = dll.NewProc("GRBgetintattr")
}

func call(p *syscall.LazyProc, what string, args ...uintptr) uintptr {
	r1, _, err := p.Call(args...)
	if int32(r1) != 0 {
		fmt.Printf("GRB FAIL at %s: code=%d lastErr=%v\n", what, int32(r1), err)
		os.Exit(1)
	}
	return r1
}

func ptr(s string) uintptr {
	b := append([]byte(s), 0)
	return uintptr(unsafe.Pointer(&b[0]))
}

func main() {
	os.Setenv("GRB_LICENSE_FILE", `D:\code\challenge-roadef-2026\.venv\Lib\site-packages\gurobipy\gurobi.lic`)
	load()

	var env, model unsafe.Pointer
	call(pLoadEnv, "loadenv", uintptr(unsafe.Pointer(&env)), 0)
	call(pNewModel, "newmodel", uintptr(env), uintptr(unsafe.Pointer(&model)), 0, 0, 0, 0, 0, 0, 0, 0)

	nvars := 2
	obj := []float64{1, 1}
	lb := []float64{0, 0}
	ub := []float64{1, 1}
	vt := []byte("CC")
	call(pAddVars, "addvars",
		uintptr(model), uintptr(nvars), 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&obj[0])),
		uintptr(unsafe.Pointer(&lb[0])),
		uintptr(unsafe.Pointer(&ub[0])),
		uintptr(unsafe.Pointer(&vt[0])), 0)

	ncon, nnz := 1, 2
	cbeg := []int32{0, 2}
	cind := []int32{0, 1}
	cval := []float64{1, 1}
	sense := []byte("<")
	rhs := []float64{1.5}
	call(pAddConstrs, "addconstrs",
		uintptr(model), uintptr(ncon), uintptr(nnz),
		uintptr(unsafe.Pointer(&cbeg[0])),
		uintptr(unsafe.Pointer(&cind[0])),
		uintptr(unsafe.Pointer(&cval[0])),
		uintptr(unsafe.Pointer(&sense[0])),
		uintptr(unsafe.Pointer(&rhs[0])), 0)

	call(pSetIntAttr, "setsense", uintptr(model), ptr("ModelSense"), ^uintptr(0))
	call(pUpdate, "update", uintptr(model))
	call(pOptimize, "optimize", uintptr(model))

	var status int32
	call(pGetIntAttr, "getstatus", uintptr(model), ptr("Status"), uintptr(unsafe.Pointer(&status)))
	xv := make([]float64, nvars)
	call(pGetDblArr, "getx", uintptr(model), ptr("X"), 0, uintptr(nvars), uintptr(unsafe.Pointer(&xv[0])))

	fmt.Printf("status=%d  x=%v  obj(x+y)=%v\n", status, xv, xv[0]+xv[1])
	if status == 2 && xv[0]+xv[1] == 1.5 {
		fmt.Println("SPIKE OK")
	} else {
		os.Exit(1)
	}
}
