// Package gurobi is a minimal wrapper around the Gurobi C API (Windows).
//
// Go's syscall package cannot pass scalar double arguments (the Windows x64
// ABI puts them in XMM registers), so only functions whose numeric arguments
// are integers or *arrays* (obj/lb/ub/vtype/constraint CSR/attributes) are
// used.  Parameters that would need a scalar double (e.g. TimeLimit) are
// avoided; callers that need a wall-clock cap should terminate from another
// goroutine (GRBterminate takes no double).
package gurobi

import (
	"fmt"
	"os"
	"sync"
	"syscall"
	"unsafe"
)

// Solver status codes (gurobi_c.h).
const (
	StatusOptimal     = 2
	StatusInfeasible  = 3
	StatusInfOrUnbd   = 4
	StatusUnbounded   = 5
	StatusTimeLimit   = 9
	StatusInterrupted = 11
	StatusNumeric     = 13
	StatusSuboptimal  = 14
)

// Objective sense (GRB_MINIMIZE = 1, GRB_MAXIMIZE = -1; the sense is the
// multiplier applied to the objective, so minimising uses +1).
const ModelSenseMinimize = 1

var (
	loadOnce sync.Once
	loadErr  error

	procLoadEnv       *syscall.LazyProc
	procNewModel      *syscall.LazyProc
	procAddVars       *syscall.LazyProc
	procAddConstrs    *syscall.LazyProc
	procSetIntParam   *syscall.LazyProc
	procSetParam      *syscall.LazyProc
	procSetIntAttr    *syscall.LazyProc
	procSetDblAttrArr *syscall.LazyProc
	procGetIntAttr    *syscall.LazyProc
	procGetDblAttr    *syscall.LazyProc
	procGetDblAttrArr *syscall.LazyProc
	procUpdate        *syscall.LazyProc
	procOptimize      *syscall.LazyProc
	procTerminate     *syscall.LazyProc
	procFreeModel     *syscall.LazyProc
	procFreeEnv       *syscall.LazyProc
	procWrite         *syscall.LazyProc
)

// dllCandidates returns likely locations of gurobi130.dll on this machine.
func dllCandidates() []string {
	return []string{
		os.Getenv("TASR_GRB_DLL"),
		`C:\Users\xudeyi\AppData\Local\Programs\gurobi1300\win64\bin\gurobi130.dll`,
		`D:\code\challenge-roadef-2026\.venv\Lib\site-packages\gurobipy\gurobi130.dll`,
		`C:\gurobi\win64\bin\gurobi130.dll`,
	}
}

// load resolves the Gurobi DLL once.  The first existing candidate wins.
func load() error {
	loadOnce.Do(func() {
		var dll *syscall.LazyDLL
		for _, p := range dllCandidates() {
			if p == "" {
				continue
			}
			if _, err := os.Stat(p); err == nil {
				dll = syscall.NewLazyDLL(p)
				break
			}
		}
		if dll == nil {
			loadErr = fmt.Errorf("gurobi130.dll not found (set TASR_GRB_DLL)")
			return
		}
		for _, pr := range []struct {
			name string
			dst  **syscall.LazyProc
		}{
			{"GRBloadenv", &procLoadEnv},
			{"GRBnewmodel", &procNewModel},
			{"GRBaddvars", &procAddVars},
			{"GRBaddconstrs", &procAddConstrs},
			{"GRBsetintparam", &procSetIntParam},
			{"GRBsetparam", &procSetParam},
			{"GRBsetintattr", &procSetIntAttr},
			{"GRBsetdblattrarray", &procSetDblAttrArr},
			{"GRBgetintattr", &procGetIntAttr},
			{"GRBgetdblattr", &procGetDblAttr},
			{"GRBgetdblattrarray", &procGetDblAttrArr},
			{"GRBupdatemodel", &procUpdate},
			{"GRBoptimize", &procOptimize},
			{"GRBterminate", &procTerminate},
			{"GRBfreemodel", &procFreeModel},
			{"GRBfreeenv", &procFreeEnv},
			{"GRBwrite", &procWrite},
		} {
			*pr.dst = dll.NewProc(pr.name)
		}
	})
	return loadErr
}

// cstr copies a Go string into a NUL-terminated C string that stays alive
// across the call.
func cstr(s string) *byte {
	b := append([]byte(s), 0)
	return &b[0]
}

// call runs a proc and maps a non-zero return code to an error.
func call(p *syscall.LazyProc, what string, args ...uintptr) error {
	r1, _, _ := p.Call(args...)
	if code := int32(r1); code != 0 {
		return fmt.Errorf("gurobi %s failed: code %d", what, code)
	}
	return nil
}

// Env is a Gurobi environment.
type Env struct {
	p unsafe.Pointer
}

// Model is a Gurobi model.
type Model struct {
	p     unsafe.Pointer
	nvars int
}

// EnvNew creates a fresh environment (empty log file name = console).
func EnvNew() (*Env, error) {
	if err := load(); err != nil {
		return nil, err
	}
	var env unsafe.Pointer
	if err := call(procLoadEnv, "GRBloadenv", uintptr(unsafe.Pointer(&env)), 0); err != nil {
		return nil, err
	}
	e := &Env{p: env}
	if err := e.SetIntParam("OutputFlag", 0); err != nil {
		e.Free()
		return nil, err
	}
	return e, nil
}

// SetIntParam sets an integer environment parameter (e.g. OutputFlag).
func (e *Env) SetIntParam(name string, v int) error {
	return call(procSetIntParam, "GRBsetintparam", uintptr(e.p), uintptr(unsafe.Pointer(cstr(name))), uintptr(int32(v)))
}

// SetParam sets an environment parameter from a string value.  This is the
// generic GRBsetparam, which accepts numeric parameters (TimeLimit, MIPGap,
// ...) as their string form and avoids passing doubles through the syscall
// boundary (the Windows x64 ABI passes floats in XMM registers).
func (e *Env) SetParam(name, value string) error {
	return call(procSetParam, "GRBsetparam", uintptr(e.p),
		uintptr(unsafe.Pointer(cstr(name))), uintptr(unsafe.Pointer(cstr(value))))
}

// NewModel builds an empty named model in env.
func (e *Env) NewModel(name string) (*Model, error) {
	var m unsafe.Pointer
	if err := call(procNewModel, "GRBnewmodel", uintptr(e.p), uintptr(unsafe.Pointer(&m)),
		uintptr(unsafe.Pointer(cstr(name))),
		0, 0, 0, 0, 0, 0); err != nil {
		return nil, err
	}
	return &Model{p: m}, nil
}

// AddVars appends numvars variables.  obj/lb/ub/vtype must have length
// numvars (vtype is 'B' binary, 'C' continuous, 'I' integer).  Column-wise
// nonzeros are not used (0), coefficients are added row-wise afterwards.
func (m *Model) AddVars(obj, lb, ub []float64, vtype []byte) (first int, err error) {
	if len(obj) != len(lb) || len(obj) != len(ub) || len(obj) != len(vtype) {
		return 0, fmt.Errorf("gurobi AddVars: length mismatch")
	}
	first = m.nvars
	err = call(procAddVars, "GRBaddvars", uintptr(m.p),
		uintptr(len(obj)), 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&obj[0])),
		uintptr(unsafe.Pointer(&lb[0])),
		uintptr(unsafe.Pointer(&ub[0])),
		uintptr(unsafe.Pointer(&vtype[0])), 0)
	if err == nil {
		m.nvars += len(obj)
	}
	return first, err
}

// NVars returns the number of variables added so far.
func (m *Model) NVars() int { return m.nvars }

// AddConstrs adds rows in CSR form.  cbeg has length ncon+1; sense/rhs have
// length ncon; cind/cval length nnz.  Empty models/arrays are handled by
// passing dedicated zeroed memory (nil pointers are not accepted).
func (m *Model) AddConstrs(cbeg []int32, cind []int32, cval []float64, sense []byte, rhs []float64) error {
	ncon := len(rhs)
	if len(cbeg) != ncon+1 {
		return fmt.Errorf("gurobi AddConstrs: cbeg len %d != ncon+1 %d", len(cbeg), ncon+1)
	}
	if len(cind) != len(cval) {
		return fmt.Errorf("gurobi AddConstrs: cind/cval length mismatch")
	}
	nnz := len(cind)
	return call(procAddConstrs, "GRBaddconstrs", uintptr(m.p),
		uintptr(ncon), uintptr(nnz),
		uintptr(unsafe.Pointer(&cbeg[0])),
		uintptr(unsafe.Pointer(&cind[0])),
		uintptr(unsafe.Pointer(&cval[0])),
		uintptr(unsafe.Pointer(&sense[0])),
		uintptr(unsafe.Pointer(&rhs[0])), 0)
}

// SetIntAttr sets an integer model attribute (e.g. ModelSense).
func (m *Model) SetIntAttr(name string, v int) error {
	return call(procSetIntAttr, "GRBsetintattr", uintptr(m.p), uintptr(unsafe.Pointer(cstr(name))), uintptr(int32(v)))
}

// SetDblAttrArray sets a double array attribute slice (Obj/LB/UB/X...).
// first is the starting variable index.
func (m *Model) SetDblAttrArray(name string, first int, vals []float64) error {
	return call(procSetDblAttrArr, "GRBsetdblattrarray", uintptr(m.p),
		uintptr(unsafe.Pointer(cstr(name))), uintptr(first), uintptr(len(vals)),
		uintptr(unsafe.Pointer(&vals[0])))
}

// IntAttr reads an integer model attribute (Status, NumVars, ...).
func (m *Model) IntAttr(name string) (int, error) {
	var v int32
	if err := call(procGetIntAttr, "GRBgetintattr", uintptr(m.p), uintptr(unsafe.Pointer(cstr(name))), uintptr(unsafe.Pointer(&v))); err != nil {
		return 0, err
	}
	return int(v), nil
}

// DblAttr reads a double model attribute (ObjVal).
func (m *Model) DblAttr(name string) (float64, error) {
	var v float64
	if err := call(procGetDblAttr, "GRBgetdblattr", uintptr(m.p), uintptr(unsafe.Pointer(cstr(name))), uintptr(unsafe.Pointer(&v))); err != nil {
		return 0, err
	}
	return v, nil
}

// GetDblAttrArray reads a double array attribute into vals.
func (m *Model) GetDblAttrArray(name string, first int, vals []float64) error {
	return call(procGetDblAttrArr, "GRBgetdblattrarray", uintptr(m.p),
		uintptr(unsafe.Pointer(cstr(name))), uintptr(first), uintptr(len(vals)),
		uintptr(unsafe.Pointer(&vals[0])))
}

// X reads the current solution values of all variables into vals.
func (m *Model) X(vals []float64) error {
	return m.GetDblAttrArray("X", 0, vals)
}

// Update syncs pending model modifications.
func (m *Model) Update() error {
	return call(procUpdate, "GRBupdatemodel", uintptr(m.p))
}

// Optimize solves the current model.
func (m *Model) Optimize() error {
	return call(procOptimize, "GRBoptimize", uintptr(m.p))
}

// Terminate asks the solver to stop (safe from another goroutine).
func (m *Model) Terminate() {
	_, _, _ = procTerminate.Call(uintptr(m.p))
}

// Write dumps the model in LP format (debugging aid).
func (m *Model) Write(path string) error {
	return call(procWrite, "GRBwrite", uintptr(m.p), uintptr(unsafe.Pointer(cstr(path))))
}

// Free releases the model; Free releases the environment.
func (m *Model) Free() {
	if m.p != nil {
		_, _, _ = procFreeModel.Call(uintptr(m.p))
		m.p = nil
	}
}

func (e *Env) Free() {
	if e.p != nil {
		_, _, _ = procFreeEnv.Call(uintptr(e.p))
		e.p = nil
	}
}
