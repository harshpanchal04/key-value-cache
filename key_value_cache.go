package main

import (
	"container/heap"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	maxKeySize   = 256
	maxValueSize = 256
)

// Item represents a cached data entry
type Item struct {
	Key        string
	Val        string
	Expiry     int64
	Size       int
	LastAccess int64
}

// ExpiryHeap manages expiration priority
type ExpiryHeap []*Item

func (h ExpiryHeap) Len() int            { return len(h) }
func (h ExpiryHeap) Less(i, j int) bool  { return h[i].Expiry < h[j].Expiry }
func (h ExpiryHeap) Swap(i, j int)       { h[i], h[j] = h[j], h[i] }
func (h *ExpiryHeap) Push(x interface{}) { *h = append(*h, x.(*Item)) }
func (h *ExpiryHeap) Pop() interface{} {
	old := *h
	n := len(old)
	itm := old[n-1]
	*h = old[:n-1]
	return itm
}

// Shard represents a partition of the cache
type Shard struct {
	entries    map[string]*Item
	lruKeys    []string
	mutex      sync.RWMutex
	memUsed    int
	expQueue   *ExpiryHeap
	accessCtr  int64
}

// Store manages multiple cache shards
type Store struct {
	shards     []*Shard
	numShards  int
	maxMem     int64
	sizeCalc   func(string) int
	totalMem   int64
	hits       int64
	misses     int64
	evictCount int64
	evictMem   int64
	storeMutex sync.RWMutex
	memMutex   sync.Mutex
	expMutex   sync.Mutex
}

// CreateStore initializes a new cache instance
func CreateStore(shardNum int, memoryLimit int64, sizeFn func(string) int) *Store {
	s := &Store{
		shards:    make([]*Shard, shardNum),
		numShards: shardNum,
		maxMem:    memoryLimit,
		sizeCalc:  sizeFn,
	}

	for i := 0; i < shardNum; i++ {
		s.shards[i] = &Shard{
			entries:  make(map[string]*Item),
			lruKeys:  make([]string, 0),
			expQueue: &ExpiryHeap{},
		}
		heap.Init(s.shards[i].expQueue)
	}

	return s
}

// locateShard finds the appropriate partition for a key
func (s *Store) locateShard(k string) *Shard {
	h := fnv.New32a()
	h.Write([]byte(k))
	return s.shards[h.Sum32()%uint32(s.numShards)]
}

// Retrieve gets a value from the cache
func (s *Store) Retrieve(k string) (string, bool) {
	shard := s.locateShard(k)
	shard.mutex.Lock()
	defer shard.mutex.Unlock()

	entry, exists := shard.entries[k]
	if !exists {
		s.storeMutex.Lock()
		s.misses++
		s.storeMutex.Unlock()
		return "", false
	}

	currentTime := time.Now().Unix()
	if currentTime > entry.Expiry {
		s.removeItem(shard, k)
		return "", false
	}

	entry.LastAccess = currentTime
	shard.accessCtr++
	s.storeMutex.Lock()
	s.hits++
	s.storeMutex.Unlock()
	return entry.Val, true
}

// Insert adds or updates an entry in the cache
func (s *Store) Insert(k, v string, ttl int) error {
	shard := s.locateShard(k)
	shard.mutex.Lock()
	defer shard.mutex.Unlock()

	entrySize := s.sizeCalc(v)
	expTime := time.Now().Unix() + int64(ttl)

	if s.totalMem+int64(entrySize) > s.maxMem {
		s.freeSpace(int64(entrySize))
		if s.totalMem+int64(entrySize) > s.maxMem {
			return fmt.Errorf("entry exceeds available memory")
		}
	}

	if existing, found := shard.entries[k]; found {
		s.memMutex.Lock()
		s.totalMem -= int64(existing.Size)
		s.totalMem += int64(entrySize)
		s.memMutex.Unlock()
		shard.memUsed -= existing.Size
		shard.memUsed += entrySize

		existing.Val = v
		existing.Expiry = expTime
		existing.Size = entrySize
		existing.LastAccess = time.Now().Unix()
		return nil
	}

	newItem := &Item{
		Key:        k,
		Val:        v,
		Expiry:     expTime,
		Size:       entrySize,
		LastAccess: time.Now().Unix(),
	}
	shard.entries[k] = newItem
	shard.lruKeys = append(shard.lruKeys, k)
	shard.memUsed += entrySize
	s.memMutex.Lock()
	s.totalMem += int64(entrySize)
	s.memMutex.Unlock()
	heap.Push(shard.expQueue, newItem)

	s.applyConstraints(shard)

	return nil
}

// Remove deletes an entry from the cache
func (s *Store) Remove(k string) {
	shard := s.locateShard(k)
	shard.mutex.Lock()
	defer shard.mutex.Unlock()
	s.removeItem(shard, k)
}

func (s *Store) removeItem(shard *Shard, k string) {
	if item, exists := shard.entries[k]; exists {
		delete(shard.entries, k)
		for i, key := range shard.lruKeys {
			if key == k {
				shard.lruKeys = append(shard.lruKeys[:i], shard.lruKeys[i+1:]...)
				break
			}
		}
		s.memMutex.Lock()
		s.totalMem -= int64(item.Size)
		s.memMutex.Unlock()
		shard.memUsed -= item.Size
		s.evictCount++
		s.evictMem += int64(item.Size)
	}
}

// applyConstraints enforces memory limits
func (s *Store) applyConstraints(shard *Shard) {
	for s.totalMem > s.maxMem {
		s.freeSpace(0)
	}
}

// freeSpace removes entries to reclaim memory
func (s *Store) freeSpace(required int64) {
	s.storeMutex.Lock()
	defer s.storeMutex.Unlock()

	var reclaimed int64
	for idx := 0; idx < s.numShards && s.totalMem-required > s.maxMem*9/10; idx++ {
		shard := s.shards[idx]
		shard.mutex.Lock()
		if len(shard.entries) == 0 {
			shard.mutex.Unlock()
			continue
		}

		now := time.Now().Unix()
		for shard.expQueue.Len() > 0 && (*shard.expQueue)[0].Expiry <= now {
			entry := heap.Pop(shard.expQueue).(*Item)
			if _, exists := shard.entries[entry.Key]; exists {
				s.removeItem(shard, entry.Key)
				reclaimed += int64(entry.Size)
			}
		}

		if s.totalMem-required-reclaimed <= s.maxMem*9/10 {
			shard.mutex.Unlock()
			break
		}

		var largestEntry *Item
		maxSize, foundIdx := 0, -1
		for pos, key := range shard.lruKeys {
			entry := shard.entries[key]
			if entry.Size > maxSize {
				maxSize = entry.Size
				largestEntry = entry
				foundIdx = pos
			}
		}

		if largestEntry != nil {
			s.removeItem(shard, largestEntry.Key)
			reclaimed += int64(largestEntry.Size)
			shard.lruKeys = append(shard.lruKeys[:foundIdx], shard.lruKeys[foundIdx+1:]...)
		}
		shard.mutex.Unlock()
	}

	s.evictCount++
	s.evictMem += reclaimed
}

var mainCache *Store

func init() {
	numShards := 32
	if envVal := os.Getenv("CACHE_SHARD_COUNT"); envVal != "" {
		if parsed, err := strconv.Atoi(envVal); err == nil && parsed > 0 {
			numShards = parsed
		}
	}

	memLimit := int64(256 * 1024 * 1024)
	if memEnv := os.Getenv("CACHE_MAX_MEMORY"); memEnv != "" {
		if parsed, err := strconv.ParseInt(memEnv, 10, 64); err == nil {
			memLimit = parsed
		}
	}

	mainCache = CreateStore(numShards, memLimit, func(v string) int { return len(v) })

	registerMetrics()
}

func registerMetrics() {
    metrics := []prometheus.Collector{
        prometheus.NewCounterFunc(prometheus.CounterOpts{
            Name: "cache_hits",
            Help: "Number of cache hits.",
        }, func() float64 { return float64(mainCache.hits) }),
        prometheus.NewCounterFunc(prometheus.CounterOpts{
            Name: "cache_misses",
            Help: "Number of cache misses.",
        }, func() float64 { return float64(mainCache.misses) }),
        prometheus.NewCounterFunc(prometheus.CounterOpts{
            Name: "cache_evictions",
            Help: "Number of cache evictions.",
        }, func() float64 { return float64(mainCache.evictCount) }),
        prometheus.NewGaugeFunc(prometheus.GaugeOpts{
            Name: "cache_memory_usage",
            Help: "Current memory usage of the cache in bytes.",
        }, func() float64 { return float64(mainCache.totalMem) }),
        prometheus.NewGaugeFunc(prometheus.GaugeOpts{
            Name: "cache_max_memory",
            Help: "Maximum memory allowed for the cache in bytes.",
        }, func() float64 { return float64(mainCache.maxMem) }),
    }

    for _, collector := range metrics {
        prometheus.MustRegister(collector)
    }
}

// HandlePut processes write requests
func HandlePut(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	var input map[string]string
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		sendResponse(w, "ERROR", "Invalid input")
		return
	}

	k, ok := input["key"]
	if !ok || len(k) > maxKeySize {
		sendResponse(w, "ERROR", "Invalid key")
		return
	}

	v, ok := input["value"]
	if !ok || len(v) > maxValueSize {
		sendResponse(w, "ERROR", "Invalid value")
		return
	}

	ttl := 60
	if ttlStr, exists := input["ttl"]; exists {
		if parsed, err := strconv.Atoi(ttlStr); err == nil && parsed > 0 {
			ttl = parsed
		}
	}

	if err := mainCache.Insert(k, v, ttl); err != nil {
		sendResponse(w, "ERROR", fmt.Sprintf("Cache error: %s", err))
		return
	}

	sendResponse(w, "OK", "Key inserted/updated successfully.")
}

// HandleGet processes read requests
func HandleGet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	k := r.URL.Query().Get("key")
	if len(k) > maxKeySize {
		sendResponse(w, "ERROR", "Invalid key")
		return
	}

	val, found := mainCache.Retrieve(k)
	if found {
		jsonResponse(w, "OK", map[string]string{"key": k, "value": val})
	} else {
		sendResponse(w, "ERROR", "Key not found")
	}
}

func sendResponse(w http.ResponseWriter, status, message string) {
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"status":  status,
		"message": message,
	})
}

func jsonResponse(w http.ResponseWriter, status string, data map[string]string) {
	w.WriteHeader(http.StatusOK)
	response := map[string]string{"status": status}
	for k, v := range data {
		response[k] = v
	}
	json.NewEncoder(w).Encode(response)
}

func main() {
	http.HandleFunc("/put", HandlePut)
	http.HandleFunc("/get", HandleGet)
	http.Handle("/metrics", promhttp.Handler())

	port := "7171"
	if envPort := os.Getenv("PORT"); envPort != "" {
		port = envPort
	}

	log.Printf("Initializing service on port %s\n", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatalf("Server initialization failed: %v", err)
	}
}