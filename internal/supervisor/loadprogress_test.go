package supervisor

import "testing"

func TestLoadProgressIsMemoryHeldOverWhatIsBeingLoaded(t *testing.T) {
	const gb = int64(1) << 30
	for _, c := range []struct {
		name          string
		held, weights int64
		onGPU         bool
		fraction      float64
		message       string
	}{
		{"the process exists and holds nothing yet", 0, 17 * gb, true, 0, "starting engine"},
		{"part of the way onto the GPU", 8*gb + gb/2, 17 * gb, true, 0.5, "loading weights onto the GPU: 8.5 of 17.0 GB"},
		{"an engine nvidia-smi does not see, a Mac's, is measured in RAM", 4 * gb, 16 * gb, false, 0.25, "loading weights into memory: 4.0 of 16.0 GB"},
		// The weights are in and the KV cache is being allocated: more than
		// the files' size is held, and the load is still not done.
		{"past the weights' size it is never called finished", 21 * gb, 17 * gb, true, 0.99, "weights loaded; preparing slots and cache"},
		{"a model whose size is not known has no fraction, only what is held", 5 * gb, 0, true, 0, "loading model: 5.0 GB so far"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := loadFraction(c.held, c.weights); got != c.fraction {
				t.Errorf("fraction %v, want %v", got, c.fraction)
			}
			if got := loadMessage(c.held, c.weights, c.onGPU); got != c.message {
				t.Errorf("message %q, want %q", got, c.message)
			}
		})
	}
}
