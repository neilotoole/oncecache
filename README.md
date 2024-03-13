# oncecache: on-demand in-memory object cache

[![Go Reference](https://pkg.go.dev/badge/github.com/neilotoole/oncecache.svg)](https://pkg.go.dev/github.com/neilotoole/oncecache)
[![Go Report Card](https://goreportcard.com/badge/neilotoole/oncecache)](https://goreportcard.com/report/neilotoole/oncecache)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](https://github.com/neilotoole/oncecache/blob/master/LICENSE)
![Workflow](https://github.com/neilotoole/oncecache/actions/workflows/go.yml/badge.svg)

`oncecache` is a strongly-typed, concurrency-safe, context-aware,
dependency-free, in-memory, on-demand Golang object cache, focused on write-once,
read-often ergonomics.

The package also provides an event mechanism useful for logging, metrics, or
propagating cache entries between overlapping composite caches.
