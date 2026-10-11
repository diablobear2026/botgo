package main

import "testing"

func TestBuildBucketURL(t *testing.T) {
	u, err := buildBucketURL("my-bucket-1250000000", "ap-guangzhou")
	if err != nil {
		t.Fatalf("不期望报错: %v", err)
	}
	want := "https://my-bucket-1250000000.cos.ap-guangzhou.myqcloud.com"
	if u.String() != want {
		t.Fatalf("got %q, want %q", u.String(), want)
	}
}

func TestBuildBucketURLEmptyBucket(t *testing.T) {
	if _, err := buildBucketURL("", "ap-guangzhou"); err == nil {
		t.Fatal("期望bucket为空时报错，实际没有报错")
	}
}
