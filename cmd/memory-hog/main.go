// memory-hog is a bounded demonstration workload for memory-tier
// observability. It allocates a fixed amount of anonymous memory, touches
// every page once (making it cold-but-resident), then keeps re-touching a
// configured hot fraction. Under node memory pressure, the kernel's tiering
// demotes the cold pages to slower tiers while the hot pages stay on DRAM —
// exactly the behavior the memory-tier metrics are designed to expose.
//
// It exits on SIGTERM/SIGINT, releasing all memory.
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const pageSize = 4096

func main() {
	var (
		totalMB  = flag.Int("total-mb", 1024, "Total anonymous memory to allocate and touch (MiB).")
		hotMB    = flag.Int("hot-mb", 64, "Fraction of memory to keep hot by re-touching (MiB).")
		interval = flag.Duration("touch-interval", 100*time.Millisecond, "How often to re-touch the hot region.")
	)
	flag.Parse()

	total := *totalMB * 1024 * 1024
	hot := *hotMB * 1024 * 1024
	if hot > total {
		log.Fatalf("--hot-mb (%d) may not exceed --total-mb (%d)", *hotMB, *totalMB)
	}

	buf := make([]byte, total)
	// First touch: every page becomes resident (cold until revisited).
	for i := 0; i < len(buf); i += pageSize {
		buf[i] = byte(i)
	}
	log.Printf("memory-hog: touched %d MiB total, keeping %d MiB hot (interval %s)",
		total/(1024*1024), hot/(1024*1024), interval)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	ticker := time.NewTicker(*interval)
	defer ticker.Stop()
	for {
		select {
		case <-sig:
			log.Printf("memory-hog: signal received, exiting")
			return
		case <-ticker.C:
			// Re-touch the hot region so kernel aging sees it hot.
			for i := 0; i < hot; i += pageSize {
				buf[i]++
			}
		}
	}
}
