package main

import "testing"

func TestLoadDedicatedPoolConfigPartitionsFixedBudget(t *testing.T) {
	t.Setenv("DB_TRANSFER_POOL_CONNS", "2")
	t.Setenv("DB_PUBLISHER_POOL_CONNS", "14")
	t.Setenv("DB_AGGREGATE_POOL_CONNS", "4")

	config, err := loadDedicatedPoolConfig(90)
	if err != nil {
		t.Fatal(err)
	}
	if config.foreground != 70 || config.transfer != 2 || config.publisher != 14 || config.aggregate != 4 {
		t.Fatalf("unexpected partition: %#v", config)
	}
}

func TestLoadDedicatedPoolConfigRejectsPartialPartition(t *testing.T) {
	t.Setenv("DB_TRANSFER_POOL_CONNS", "2")
	t.Setenv("DB_PUBLISHER_POOL_CONNS", "0")
	t.Setenv("DB_AGGREGATE_POOL_CONNS", "4")

	if _, err := loadDedicatedPoolConfig(90); err == nil {
		t.Fatal("expected partial dedicated pool configuration to fail")
	}
}

func TestLoadDedicatedPoolConfigRejectsOversubscription(t *testing.T) {
	t.Setenv("DB_TRANSFER_POOL_CONNS", "20")
	t.Setenv("DB_PUBLISHER_POOL_CONNS", "60")
	t.Setenv("DB_AGGREGATE_POOL_CONNS", "10")

	if _, err := loadDedicatedPoolConfig(90); err == nil {
		t.Fatal("expected connection budget oversubscription to fail")
	}
}
