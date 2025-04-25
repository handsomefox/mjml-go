package mjml

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/bytecodealliance/wasmtime-go/v32"
	"github.com/jackc/puddle/v2"
)

func constructor(ctx context.Context) (*StoreInstance, error) {
	id, err := randomIdentifier()
	if err != nil {
		return nil, fmt.Errorf("error generating random id for wasm module: %w", err)
	}

	idStr := strconv.Itoa(int(id))

	store := wasmtime.NewStore(engine)

	wasiConfig := wasmtime.NewWasiConfig()
	wasiConfig.InheritStdin()
	wasiConfig.InheritStdout()
	wasiConfig.InheritStderr()
	store.SetWasi(wasiConfig)

	instance, err := linker.Instantiate(store, wasmModule)
	if err != nil {
		return nil, fmt.Errorf("error instantiating wasm module %s: %w", idStr, err)
	}

	memory := instance.GetExport(store, "memory").Memory()
	if memory == nil {
		return nil, fmt.Errorf("memory not found in wasm module %s", idStr)
	}

	return &StoreInstance{
		Store:    store,
		Instance: instance,
		Memory:   memory,
	}, nil
}

func destructor(si *StoreInstance) {
}

func newResourcePool(maxSize int32) (*puddle.Pool[*StoreInstance], error) {
	pool, err := puddle.NewPool(&puddle.Config[*StoreInstance]{
		Constructor: constructor,
		Destructor:  destructor,
		MaxSize:     maxSize,
	})
	if err != nil {
		return pool, fmt.Errorf("error creating resource pool: %w", err)
	}

	err = pool.CreateResource(context.Background())
	if err != nil {
		return pool, fmt.Errorf("error prewarming resource pool: %w", err)
	}

	return pool, nil
}

func periodicallyRemoveIdleResources(pool *puddle.Pool[*StoreInstance]) {
	duration := 2 * time.Second
	ticker := time.NewTicker(duration)

	for range ticker.C {
		stats := pool.Stat()

		if stats.TotalResources() <= 1 {
			continue
		}

		idleResources := pool.AcquireAllIdle()
		numIdleResources := len(idleResources)

		if numIdleResources <= 0 {
			continue
		}

		max := int(stats.TotalResources())

		amountToKill := numIdleResources

		if numIdleResources >= max {
			amountToKill = numIdleResources - 1
		}

		for i := 0; i < numIdleResources; i++ {
			if i >= amountToKill {
				idleResources[i].Release()
			} else {
				idleResources[i].Destroy()
			}
		}
	}
}
