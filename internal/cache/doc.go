// Package cache is the object cache of the cache and daemon tiers (09 §Caching
// tiers, M8): a Tier answers ObjectService reads from a memory LRU and an
// optional disk LRU, and fetches misses from one upstream ObjectService,
// collapsing concurrent misses and batching them.
package cache
