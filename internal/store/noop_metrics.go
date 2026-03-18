package store

// NoopStoreMetrics is a StoreMetrics implementation where all methods are
// no-ops. Use it in tests or when metrics collection is not required.
type NoopStoreMetrics struct{}

var _ StoreMetrics = NoopStoreMetrics{}

func (NoopStoreMetrics) ObserveQueryDuration(string, float64) {}
func (NoopStoreMetrics) IncQueryError(string)                 {}
func (NoopStoreMetrics) SetPoolStats(int32, int32, int32)     {}
