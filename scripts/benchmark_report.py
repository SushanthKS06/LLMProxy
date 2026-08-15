#!/usr/bin/env python3
import sys
import re

def main():
    print("Reading benchmark results...")
    content = sys.stdin.read()
    
    # Matches Go benchmark output like:
    # BenchmarkSemanticCache/hit_exact-8         10000            12345 ns/op         100 B/op          2 allocs/op
    bench_re = re.compile(r'^(Benchmark\w+[\w/-]+)\s+(\d+)\s+([\d.]+)\s+ns/op\s+(\d+)\s+B/op\s+(\d+)\s+allocs/op', re.MULTILINE)
    
    results = bench_re.findall(content)
    if not results:
        print("No benchmark results found. Run: go test -bench=. ./... | python scripts/benchmark_report.py")
        return
        
    print(f"{'Benchmark Name':<45} | {'Ops':<10} | {'Latency (µs)':<12} | {'Bytes/op':<10} | {'Allocs/op':<10}")
    print("-" * 100)
    
    for name, iters, ns_op, bytes_op, allocs_op in results:
        latency_us = float(ns_op) / 1000.0
        print(f"{name:<45} | {iters:<10} | {latency_us:<12.2f} | {bytes_op:<10} | {allocs_op:<10}")

if __name__ == '__main__':
    main()
