package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// runDeltaSync は delta-sync サブコマンドの本体。
// regions ファイル（"offset length type" 行形式）に従い、NFS マウントした snapshot の
// vmdisk ファイル（source-file）から該当オフセットを os.ReadAt で読み、target
// （disk.img または block device）へ同オフセットで書き込む。
//
// Prism v3 images/file は HTTP Range 非対応（Range を無視して全ファイルを返す）ため、
// 差分だけを取得できない。そこで snapshot のストレージコンテナを NFS(RO) で直接マウントし、
// ファイルのランダム read（os.ReadAt = pread）で差分リージョンだけを読む方式に切り替えた。
// AHVMigration の CBT 差分同期 Job として operator イメージ内で実行される。
func runDeltaSync(args []string) int {
	fs := flag.NewFlagSet("delta-sync", flag.ExitOnError)
	sourceFile := fs.String("source-file", "", "NFS マウントした snapshot vmdisk ファイルのパス")
	regionsPath := fs.String("regions", "/config/regions", "regions file path (offset length type per line)")
	target := fs.String("target", "/target/disk.img", "target file or block device")
	block := fs.Bool("block", false, "target is a block device")
	workers := fs.Int("workers", 4, "concurrent region copies")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *sourceFile == "" {
		fmt.Fprintln(os.Stderr, "delta-sync: --source-file is required")
		return 2
	}

	regions, err := loadRegions(*regionsPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "delta-sync: load regions: %v\n", err)
		return 1
	}
	var total int64
	for _, r := range regions {
		total += r.length
	}
	fmt.Printf("delta-sync: %d regions, %d bytes total, src=%s -> %s\n", len(regions), total, *sourceFile, *target)

	// 読み出し元（NFS 上の snapshot vmdisk）を read-only で open
	src, err := os.Open(*sourceFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "delta-sync: open source: %v\n", err)
		return 1
	}
	defer src.Close()

	// 書き込み先（既存の disk.img / block device）を seek 書き込みで open（truncate しない）
	out, err := os.OpenFile(*target, os.O_WRONLY, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "delta-sync: open target: %v\n", err)
		return 1
	}
	defer out.Close()
	_ = block // target のモードで挙動は変わらない（file/block とも ReadAt/WriteAt で同一）

	type result struct {
		idx int
		err error
	}
	sem := make(chan struct{}, *workers)
	results := make(chan result, len(regions))
	var done int64

	for i := range regions {
		sem <- struct{}{}
		go func(i int) {
			defer func() { <-sem }()
			var err error
			for attempt := 1; attempt <= 3; attempt++ {
				err = syncRegion(src, out, regions[i])
				if err == nil {
					break
				}
				fmt.Fprintf(os.Stderr, "delta-sync: region %d attempt %d: %v\n", i, attempt, err)
				time.Sleep(time.Duration(attempt) * 2 * time.Second)
			}
			results <- result{i, err}
		}(i)
	}

	failed := 0
	for range regions {
		res := <-results
		if res.err != nil {
			failed++
		} else {
			done += regions[res.idx].length
		}
	}
	if failed > 0 {
		fmt.Fprintf(os.Stderr, "delta-sync: %d regions failed\n", failed)
		return 1
	}
	if err := out.Sync(); err != nil {
		fmt.Fprintf(os.Stderr, "delta-sync: fsync: %v\n", err)
		return 1
	}
	fmt.Printf("delta-sync: complete, %d bytes written\n", done)
	return 0
}

type deltaRegion struct {
	offset, length int64
	typ            string
}

func loadRegions(path string) ([]deltaRegion, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var regions []deltaRegion
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var r deltaRegion
		if _, err := fmt.Sscanf(line, "%d %d %s", &r.offset, &r.length, &r.typ); err != nil {
			return nil, fmt.Errorf("parse line %q: %w", line, err)
		}
		regions = append(regions, r)
	}
	return regions, sc.Err()
}

// syncRegion は 1 リージョンを転送する。ZEROED はゼロ書き込み、REGULAR は
// snapshot ファイルから ReadAt（pread）で読み、target へ WriteAt（pwrite）する。
// *os.File の ReadAt/WriteAt は共有オフセットを持たず並行実行に安全。
func syncRegion(src, out *os.File, r deltaRegion) error {
	if r.typ == "ZEROED" {
		return writeZeros(out, r.offset, r.length)
	}
	buf := make([]byte, 4*1024*1024)
	var done int64
	for done < r.length {
		want := int64(len(buf))
		if r.length-done < want {
			want = r.length - done
		}
		n, err := src.ReadAt(buf[:want], r.offset+done)
		if n > 0 {
			if _, werr := out.WriteAt(buf[:n], r.offset+done); werr != nil {
				return werr
			}
			done += int64(n)
		}
		if err != nil {
			if (err == io.EOF || err == io.ErrUnexpectedEOF) && done >= r.length {
				break
			}
			return fmt.Errorf("read at %d: %w", r.offset+done, err)
		}
	}
	if done != r.length {
		return fmt.Errorf("short read: %d of %d bytes at offset %d", done, r.length, r.offset)
	}
	return nil
}

func writeZeros(out *os.File, offset, length int64) error {
	zeros := make([]byte, 4*1024*1024)
	var written int64
	for written < length {
		want := int64(len(zeros))
		if length-written < want {
			want = length - written
		}
		if _, err := out.WriteAt(zeros[:want], offset+written); err != nil {
			return err
		}
		written += want
	}
	return nil
}
