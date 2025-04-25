package mjml

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
	"unsafe"

	"github.com/andybalholm/brotli"
	"github.com/bytecodealliance/wasmtime-go/v32"
	"github.com/jackc/puddle/v2"
)

//go:embed wasm/mjml.wasm.br
var wasm []byte

var (
	engine       *wasmtime.Engine
	wasmModule   *wasmtime.Module
	results      *sync.Map
	resourcePool *puddle.Pool[*StoreInstance]
	linker       *wasmtime.Linker
)

type StoreInstance struct {
	Store    *wasmtime.Store
	Instance *wasmtime.Instance
	Memory   *wasmtime.Memory
}

func init() {
	results = &sync.Map{}

	br := brotli.NewReader(bytes.NewReader(wasm))
	decompressed, err := io.ReadAll(br)
	if err != nil {
		panic(fmt.Sprintf("Error decompressing wasm file: %s", err))
	}

	engine = wasmtime.NewEngine()

	linker = wasmtime.NewLinker(engine)
	err = linker.DefineWasi()
	if err != nil {
		panic(fmt.Sprintf("Error defining WASI: %s", err))
	}

	err = registerHostFunctions(linker)
	if err != nil {
		panic(fmt.Sprintf("Error registering host functions: %s", err))
	}

	wasmModule, err = wasmtime.NewModule(engine, decompressed)
	if err != nil {
		panic(fmt.Sprintf("Error compiling wasm module: %s", err))
	}

	resourcePool, err = newResourcePool(10)
	if err != nil {
		panic(fmt.Sprintf("Error creating resource pool: %s", err))
	}

	go periodicallyRemoveIdleResources(resourcePool)
}

func SetMaxWorkers(maxSize int32) error {
	oldPool := resourcePool

	newPool, err := newResourcePool(maxSize)
	if err != nil {
		return fmt.Errorf("error creating new resource pool: %w", err)
	}

	resourcePool = newPool
	oldPool.Close()

	return nil
}

type jsonResult struct {
	HTML  string `json:"html"`
	Error *Error `json:"error,omitempty"`
}

func ToHTML(ctx context.Context, mjml string, toHTMLOptions ...ToHTMLOption) (string, error) {
	data := map[string]interface{}{
		"mjml": mjml,
	}

	o := options{
		data: map[string]interface{}{},
	}

	for _, opt := range toHTMLOptions {
		opt(o)
	}

	if len(o.data) > 0 {
		data["options"] = o.data
	}

	inputBytes := bytes.NewBuffer([]byte{})

	encoder := json.NewEncoder(inputBytes)
	encoder.SetEscapeHTML(false)

	err := encoder.Encode(data)
	if err != nil {
		return "", fmt.Errorf("error encoding input data: %w", err)
	}

	jsonInput := inputBytes.String()
	jsonInputLen := len(jsonInput)

	var (
		resource *puddle.Resource[*StoreInstance]
		tries    int
	)

	for {
		tries++

		var err error

		resource, err = resourcePool.Acquire(ctx)
		if err != nil {
			if tries >= 30 {
				return "", fmt.Errorf("unable to acquire wasm module after 30 tries: %w", err)
			}

			if err == puddle.ErrClosedPool {
				time.Sleep(1 * time.Millisecond)
				continue
			}

			return "", fmt.Errorf("error acquiring wasm module: %w", err)
		}

		break
	}

	defer resource.Release()

	si := resource.Value()
	if si == nil {
		return "", errors.New("pool resource is nil")
	}

	store := si.Store
	instance := si.Instance
	memory := si.Memory

	allocate := instance.GetFunc(store, "allocate")
	deallocate := instance.GetFunc(store, "deallocate")
	run := instance.GetFunc(store, "run_e")

	if allocate == nil || deallocate == nil || run == nil {
		return "", errors.New("required functions not found in WebAssembly module")
	}

	inputPtrUnsafe, err := allocate.Call(store, int32(jsonInputLen))
	if err != nil {
		return "", fmt.Errorf("error allocating memory: %w", err)
	}
	inputPtr, ok := inputPtrUnsafe.(int32)
	if !ok {
		return "", errors.New("invalid pointer returned from allocate")
	}

	mem_slice := getMemorySlice(memory, store)
	if int(inputPtr)+jsonInputLen > len(mem_slice) {
		return "", errors.New("memory allocation out of bounds")
	}

	copy(mem_slice[inputPtr:inputPtr+int32(jsonInputLen)], []byte(jsonInput))

	ident, err := randomIdentifier()
	if err != nil {
		return "", fmt.Errorf("error generating identifier: %w", err)
	}

	resultCh := make(chan []byte, 1)
	results.Store(ident, resultCh)
	defer results.Delete(ident)

	_, err = run.Call(store, inputPtr, int32(jsonInputLen), ident)
	if err != nil {
		return "", fmt.Errorf("error calling run: %w", err)
	}

	defer deallocate.Call(store, inputPtr)

	result := <-resultCh

	res := jsonResult{}
	err = json.Unmarshal(result, &res)
	if err != nil {
		return "", fmt.Errorf("error decoding result json: %w", err)
	}

	if res.Error != nil {
		return "", *res.Error
	}

	return res.HTML, nil
}

func getMemorySlice(memory *wasmtime.Memory, ctx interface{}) []byte {
	var dataPtr unsafe.Pointer
	var size uintptr

	switch c := ctx.(type) {
	case *wasmtime.Store:
		dataPtr = memory.Data(c)
		size = memory.DataSize(c)
	case *wasmtime.Caller:
		dataPtr = memory.Data(c)
		size = memory.DataSize(c)
	default:
		panic("invalid context type for getMemorySlice")
	}

	return unsafe.Slice((*byte)(dataPtr), size)
}

func registerHostFunctions(linker *wasmtime.Linker) error {
	err := linker.FuncWrap("env", "return_result", func(caller *wasmtime.Caller, ptr int32, length int32, ident int32) {
		memory := caller.GetExport("memory").Memory()
		if memory == nil {
			return
		}

		if ch, ok := results.Load(ident); ok {
			resultCh, isResultCh := ch.(chan []byte)
			if !isResultCh {
				return
			}

			// Get memory bytes and copy the result
			data_slice := getMemorySlice(memory, caller)
			if int(ptr)+int(length) > len(data_slice) {
				return
			}

			result := make([]byte, length)
			copy(result, data_slice[ptr:ptr+length])
			resultCh <- result
		}
	})
	if err != nil {
		return fmt.Errorf("failed to register return_result: %w", err)
	}

	err = linker.FuncWrap("env", "get_static_file", func(caller *wasmtime.Caller, ptr, len, ident int32) int32 {
		panic("get_static_file is unimplemented")
	})
	if err != nil {
		return fmt.Errorf("failed to register get_static_file: %w", err)
	}

	err = linker.FuncWrap("env", "request_set_field", func(caller *wasmtime.Caller, a, b, c, d, e, f int32) int32 {
		panic("request_set_field is unimplemented")
	})
	if err != nil {
		return fmt.Errorf("failed to register request_set_field: %w", err)
	}

	err = linker.FuncWrap("env", "resp_set_header", func(caller *wasmtime.Caller, a, b, c, d, e int32) {
		panic("resp_set_header is unimplemented")
	})
	if err != nil {
		return fmt.Errorf("failed to register resp_set_header: %w", err)
	}

	err = linker.FuncWrap("env", "cache_get", func(caller *wasmtime.Caller, a, b, c int32) int32 {
		panic("cache_get is unimplemented")
	})
	if err != nil {
		return fmt.Errorf("failed to register cache_get: %w", err)
	}

	err = linker.FuncWrap("env", "add_ffi_var", func(caller *wasmtime.Caller, a, b, c, d, e int32) int32 {
		panic("add_ffi_var is unimplemented")
	})
	if err != nil {
		return fmt.Errorf("failed to register add_ffi_var: %w", err)
	}

	err = linker.FuncWrap("env", "get_ffi_result", func(caller *wasmtime.Caller, a, b int32) int32 {
		panic("get_ffi_result is unimplemented")
	})
	if err != nil {
		return fmt.Errorf("failed to register get_ffi_result: %w", err)
	}

	err = linker.FuncWrap("env", "return_error", func(caller *wasmtime.Caller, a, b, c, d int32) {
		panic("return_error is unimplemented")
	})
	if err != nil {
		return fmt.Errorf("failed to register return_error: %w", err)
	}

	err = linker.FuncWrap("env", "fetch_url", func(caller *wasmtime.Caller, a, b, c, d, e, f int32) int32 {
		panic("fetch_url is unimplemented")
	})
	if err != nil {
		return fmt.Errorf("failed to register fetch_url: %w", err)
	}

	err = linker.FuncWrap("env", "graphql_query", func(caller *wasmtime.Caller, a, b, c, d, e int32) int32 {
		panic("graphql_query is unimplemented")
	})
	if err != nil {
		return fmt.Errorf("failed to register graphql_query: %w", err)
	}

	err = linker.FuncWrap("env", "db_exec", func(caller *wasmtime.Caller, a, b, c, d int32) int32 {
		panic("db_exec is unimplemented")
	})
	if err != nil {
		return fmt.Errorf("failed to register db_exec: %w", err)
	}

	err = linker.FuncWrap("env", "cache_set", func(caller *wasmtime.Caller, a, b, c, d, e, f int32) int32 {
		panic("cache_set is unimplemented")
	})
	if err != nil {
		return fmt.Errorf("failed to register cache_set: %w", err)
	}

	err = linker.FuncWrap("env", "request_get_field", func(caller *wasmtime.Caller, a, b, c, d int32) int32 {
		panic("request_get_field is unimplemented")
	})
	if err != nil {
		return fmt.Errorf("failed to register request_get_field: %w", err)
	}

	err = linker.FuncWrap("env", "log_msg", func(caller *wasmtime.Caller, ptr, size, level, ident int32) {
		panic("log_msg is unimplemented")
	})
	if err != nil {
		return fmt.Errorf("failed to register log_msg: %w", err)
	}

	return nil
}
