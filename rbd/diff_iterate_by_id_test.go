//go:build ceph_preview

package rbd

import (
	"sync"
	"testing"
	"time"

	"github.com/ceph/go-ceph/internal/dlsym"
	"github.com/ceph/go-ceph/rados"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDiffIterateByID(t *testing.T) {
	_, diffIterateByIdErr := dlsym.LookupSymbol("rbd_diff_iterate3")
	if diffIterateByIdErr != nil {
		t.Logf("skipping DiffIterateByID tests: rbd_diff_iterate3 not found: %v", diffIterateByIdErr)

		return
	}

	conn := radosConnect(t)
	defer conn.Shutdown()

	poolname := GetUUID()
	err := conn.MakePool(poolname)
	assert.NoError(t, err)
	defer conn.DeletePool(poolname)

	ioctx, err := conn.OpenIOContext(poolname)
	require.NoError(t, err)
	defer ioctx.Destroy()

	t.Run("basic", func(t *testing.T) {
		testDiffIterateByIDBasic(t, ioctx)
	})
	t.Run("twoAtOnce", func(t *testing.T) {
		testDiffIterateByIDTwoAtOnce(t, ioctx)
	})
	t.Run("earlyExit", func(t *testing.T) {
		testDiffIterateByIDEarlyExit(t, ioctx)
	})
	t.Run("snapshot", func(t *testing.T) {
		testDiffIterateByIDSnapshot(t, ioctx)
	})
	t.Run("callbackData", func(t *testing.T) {
		testDiffIterateByIDCallbackData(t, ioctx)
	})
	t.Run("badImage", func(t *testing.T) {
		var gotCalled int
		img := GetImage(ioctx, "bob")
		err := img.DiffIterateByID(
			DiffIterateByIDConfig{
				Offset: 0,
				Length: uint64(1 << 22),
				Callback: func(_, _ uint64, _ int, _ interface{}) int {
					gotCalled++
					return 0
				},
			})
		assert.Error(t, err)
		assert.EqualValues(t, 0, gotCalled)
	})
	t.Run("missingCallback", func(t *testing.T) {
		name := GetUUID()
		isize := uint64(1 << 23) // 8MiB
		iorder := 20             // 1MiB
		options := NewRbdImageOptions()
		defer options.Destroy()
		assert.NoError(t,
			options.SetUint64(RbdImageOptionOrder, uint64(iorder)))
		err := CreateImage(ioctx, name, isize, options)
		assert.NoError(t, err)

		img, err := OpenImage(ioctx, name, NoSnapshot)
		assert.NoError(t, err)
		defer func() {
			assert.NoError(t, img.Close())
			assert.NoError(t, img.Remove())
		}()

		var gotCalled int
		err = img.DiffIterateByID(
			DiffIterateByIDConfig{
				Offset: 0,
				Length: uint64(1 << 22),
			})
		assert.Error(t, err)
		assert.EqualValues(t, 0, gotCalled)
	})
}

func testDiffIterateByIDBasic(t *testing.T, ioctx *rados.IOContext) {
	name := GetUUID()
	isize := uint64(1 << 23) // 8MiB
	iorder := 20             // 1MiB
	options := NewRbdImageOptions()
	defer options.Destroy()
	assert.NoError(t,
		options.SetUint64(RbdImageOptionOrder, uint64(iorder)))
	err := CreateImage(ioctx, name, isize, options)
	assert.NoError(t, err)

	img, err := OpenImage(ioctx, name, NoSnapshot)
	assert.NoError(t, err)
	defer func() {
		assert.NoError(t, img.Close())
		assert.NoError(t, img.Remove())
	}()

	type diResult struct {
		offset uint64
		length uint64
	}
	calls := []diResult{}

	err = img.DiffIterateByID(
		DiffIterateByIDConfig{
			Offset: 0,
			Length: isize,
			Callback: func(o, l uint64, _ int, _ interface{}) int {
				calls = append(calls, diResult{offset: o, length: l})
				return 0
			},
		})
	assert.NoError(t, err)
	// Image is new, empty. Callback will not be called
	assert.Len(t, calls, 0)

	_, err = img.WriteAt([]byte("sometimes you feel like a nut"), 0)
	assert.NoError(t, err)

	err = img.DiffIterateByID(
		DiffIterateByIDConfig{
			Offset: 0,
			Length: isize,
			Callback: func(o, l uint64, _ int, _ interface{}) int {
				calls = append(calls, diResult{offset: o, length: l})
				return 0
			},
		})
	assert.NoError(t, err)
	if assert.Len(t, calls, 1) {
		assert.EqualValues(t, 0, calls[0].offset)
		assert.EqualValues(t, 29, calls[0].length)
	}

	_, err = img.WriteAt([]byte("sometimes you don't"), 32)
	assert.NoError(t, err)

	calls = []diResult{}
	err = img.DiffIterateByID(
		DiffIterateByIDConfig{
			Offset: 0,
			Length: isize,
			Callback: func(o, l uint64, _ int, _ interface{}) int {
				calls = append(calls, diResult{offset: o, length: l})
				return 0
			},
		})
	if assert.NoError(t, err) {
		assert.Len(t, calls, 1)
		assert.EqualValues(t, 0, calls[0].offset)
		assert.EqualValues(t, 51, calls[0].length)
	}

	// dirty a 2nd chunk
	newOffset := 3145728 // 3MiB
	_, err = img.WriteAt([]byte("alright, alright, alright"), int64(newOffset))
	assert.NoError(t, err)

	calls = []diResult{}
	err = img.DiffIterateByID(
		DiffIterateByIDConfig{
			Offset: 0,
			Length: isize,
			Callback: func(o, l uint64, _ int, _ interface{}) int {
				calls = append(calls, diResult{offset: o, length: l})
				return 0
			},
		})
	assert.NoError(t, err)
	if assert.Len(t, calls, 2) {
		assert.EqualValues(t, 0, calls[0].offset)
		assert.EqualValues(t, 51, calls[0].length)
		assert.EqualValues(t, newOffset, calls[1].offset)
		assert.EqualValues(t, 25, calls[1].length)
	}

	// dirty a 3rd chunk
	newOffset2 := 5242880 + 1024 // 5MiB + 1KiB
	_, err = img.WriteAt([]byte("zowie!"), int64(newOffset2))
	assert.NoError(t, err)

	calls = []diResult{}
	err = img.DiffIterateByID(
		DiffIterateByIDConfig{
			Offset: 0,
			Length: isize,
			Callback: func(o, l uint64, _ int, _ interface{}) int {
				calls = append(calls, diResult{offset: o, length: l})
				return 0
			},
		})
	assert.NoError(t, err)
	if assert.Len(t, calls, 3) {
		assert.EqualValues(t, 0, calls[0].offset)
		assert.EqualValues(t, 51, calls[0].length)
		assert.EqualValues(t, newOffset, calls[1].offset)
		assert.EqualValues(t, 25, calls[1].length)
		assert.EqualValues(t, newOffset2-1024, calls[2].offset)
		assert.EqualValues(t, 6+1024, calls[2].length)
	}
}

// testDiffIterateByIDTwoAtOnce aims to verify that multiple DiffIterateByID
// callbacks can be executed at the same time without error.
func testDiffIterateByIDTwoAtOnce(t *testing.T, ioctx *rados.IOContext) {
	isize := uint64(1 << 23) // 8MiB
	iorder := 20             // 1MiB
	options := NewRbdImageOptions()
	defer options.Destroy()
	assert.NoError(t,
		options.SetUint64(RbdImageOptionOrder, uint64(iorder)))

	name1 := GetUUID()
	err := CreateImage(ioctx, name1, isize, options)
	assert.NoError(t, err)

	img1, err := OpenImage(ioctx, name1, NoSnapshot)
	assert.NoError(t, err)
	defer func() {
		assert.NoError(t, img1.Close())
		assert.NoError(t, img1.Remove())
	}()

	name2 := GetUUID()
	err = CreateImage(ioctx, name2, isize, options)
	assert.NoError(t, err)

	img2, err := OpenImage(ioctx, name2, NoSnapshot)
	assert.NoError(t, err)
	defer func() {
		assert.NoError(t, img2.Close())
		assert.NoError(t, img2.Remove())
	}()

	type diResult struct {
		offset uint64
		length uint64
	}

	diffTest := func(wg *sync.WaitGroup, inbuf []byte, img *Image) {
		_, err = img.WriteAt(inbuf[0:3], 0)
		assert.NoError(t, err)
		_, err = img.WriteAt(inbuf[3:6], 3145728)
		assert.NoError(t, err)
		_, err = img.WriteAt(inbuf[6:9], 5242880)
		assert.NoError(t, err)

		calls := []diResult{}
		err = img.DiffIterateByID(
			DiffIterateByIDConfig{
				Offset: 0,
				Length: isize,
				Callback: func(o, l uint64, _ int, _ interface{}) int {
					time.Sleep(8 * time.Millisecond)
					calls = append(calls, diResult{offset: o, length: l})
					return 0
				},
			})
		assert.NoError(t, err)
		if assert.Len(t, calls, 3) {
			assert.EqualValues(t, 0, calls[0].offset)
			assert.EqualValues(t, 3, calls[0].length)
			assert.EqualValues(t, 3145728, calls[1].offset)
			assert.EqualValues(t, 3, calls[1].length)
			assert.EqualValues(t, 5242880, calls[2].offset)
			assert.EqualValues(t, 3, calls[2].length)
		}

		wg.Done()
	}

	wg := &sync.WaitGroup{}
	wg.Add(1)
	go diffTest(wg, []byte("foobarbaz"), img1)
	wg.Add(1)
	go diffTest(wg, []byte("abcdefghi"), img2)
	wg.Wait()
}

// testDiffIterateByIDEarlyExit checks that returning an error from the callback
// function triggers the DiffIterateByID call to stop.
func testDiffIterateByIDEarlyExit(t *testing.T, ioctx *rados.IOContext) {
	isize := uint64(1 << 23) // 8MiB
	iorder := 20             // 1MiB
	options := NewRbdImageOptions()
	defer options.Destroy()
	assert.NoError(t,
		options.SetUint64(RbdImageOptionOrder, uint64(iorder)))

	name := GetUUID()
	err := CreateImage(ioctx, name, isize, options)
	assert.NoError(t, err)

	img, err := OpenImage(ioctx, name, NoSnapshot)
	assert.NoError(t, err)
	defer func() {
		assert.NoError(t, img.Close())
		assert.NoError(t, img.Remove())
	}()

	type diResult struct {
		offset uint64
		length uint64
	}

	// "damage" the image
	inbuf := []byte("xxxyyyzzz")
	_, err = img.WriteAt(inbuf[0:3], 0)
	assert.NoError(t, err)
	_, err = img.WriteAt(inbuf[3:6], 3145728)
	assert.NoError(t, err)
	_, err = img.WriteAt(inbuf[6:9], 5242880)
	assert.NoError(t, err)

	// if the offset is less than zero the callback will return an "error" and
	// that will abort the DiffIterateByID call early and it will return the error
	// code our callback used.
	calls := []diResult{}
	err = img.DiffIterateByID(
		DiffIterateByIDConfig{
			Offset: 0,
			Length: isize,
			Callback: func(o, l uint64, _ int, _ interface{}) int {
				if o > 1 {
					return -5
				}
				calls = append(calls, diResult{offset: o, length: l})
				return 0
			},
		})
	assert.Error(t, err)
	if errno, ok := err.(interface{ ErrorCode() int }); assert.True(t, ok) {
		assert.EqualValues(t, -5, errno.ErrorCode())
	}
	if assert.Len(t, calls, 1) {
		assert.EqualValues(t, 0, calls[0].offset)
		assert.EqualValues(t, 3, calls[0].length)
	}
}

func testDiffIterateByIDSnapshot(t *testing.T, ioctx *rados.IOContext) {
	name := GetUUID()
	isize := uint64(1 << 23) // 8MiB
	iorder := 20             // 1MiB
	options := NewRbdImageOptions()
	defer options.Destroy()
	assert.NoError(t,
		options.SetUint64(RbdImageOptionOrder, uint64(iorder)))
	err := CreateImage(ioctx, name, isize, options)
	assert.NoError(t, err)

	img, err := OpenImage(ioctx, name, NoSnapshot)
	assert.NoError(t, err)
	defer func() {
		assert.NoError(t, img.Close())
		assert.NoError(t, img.Remove())
	}()

	type diResult struct {
		offset uint64
		length uint64
	}
	calls := []diResult{}

	err = img.DiffIterateByID(
		DiffIterateByIDConfig{
			Offset: 0,
			Length: isize,
			Callback: func(o, l uint64, _ int, _ interface{}) int {
				calls = append(calls, diResult{offset: o, length: l})
				return 0
			},
		})
	assert.NoError(t, err)
	// Image is new, empty. Callback will not be called
	assert.Len(t, calls, 0)

	_, err = img.WriteAt([]byte("sometimes you feel like a nut"), 0)
	assert.NoError(t, err)

	calls = []diResult{}
	err = img.DiffIterateByID(
		DiffIterateByIDConfig{
			Offset: 0,
			Length: isize,
			Callback: func(o, l uint64, _ int, _ interface{}) int {
				calls = append(calls, diResult{offset: o, length: l})
				return 0
			},
		})
	assert.NoError(t, err)
	if assert.Len(t, calls, 1) {
		assert.EqualValues(t, 0, calls[0].offset)
		assert.EqualValues(t, 29, calls[0].length)
	}

	ss1, err := img.CreateSnapshot("ss1")
	assert.NoError(t, err)
	defer func() { assert.NoError(t, ss1.Remove()) }()

	ss1ID, err := ss1.image.GetSnapID(ss1.name)
	assert.NoError(t, err)

	// there should be no differences between "now" and "ss1"
	calls = []diResult{}
	err = img.DiffIterateByID(
		DiffIterateByIDConfig{
			FromSnapID: ss1ID,
			Offset:     0,
			Length:     isize,
			Callback: func(o, l uint64, _ int, _ interface{}) int {
				calls = append(calls, diResult{offset: o, length: l})
				return 0
			},
		})
	assert.NoError(t, err)
	assert.Len(t, calls, 0)

	// this next check was shamelessly cribbed from the pybind
	// tests for diff_iterate out of the ceph tree
	// it discards the current image, makes a 2nd snap, and compares
	// the diff between snapshots 1 & 2.
	_, err = img.Discard(0, isize)
	assert.NoError(t, err)

	ss2, err := img.CreateSnapshot("ss2")
	assert.NoError(t, err)
	defer func() { assert.NoError(t, ss2.Remove()) }()

	err = ss2.Set() // caution: this side-effects img!
	assert.NoError(t, err)

	calls = []diResult{}
	err = img.DiffIterateByID(
		DiffIterateByIDConfig{
			FromSnapID: ss1ID,
			Offset:     0,
			Length:     isize,
			Callback: func(o, l uint64, _ int, _ interface{}) int {
				calls = append(calls, diResult{offset: o, length: l})
				return 0
			},
		})
	assert.NoError(t, err)
	if assert.Len(t, calls, 1) {
		assert.EqualValues(t, 0, calls[0].offset)
		assert.EqualValues(t, 29, calls[0].length)
	}
}

func testDiffIterateByIDCallbackData(t *testing.T, ioctx *rados.IOContext) {
	name := GetUUID()
	isize := uint64(1 << 23) // 8MiB
	iorder := 20             // 1MiB
	options := NewRbdImageOptions()
	defer options.Destroy()
	assert.NoError(t,
		options.SetUint64(RbdImageOptionOrder, uint64(iorder)))
	err := CreateImage(ioctx, name, isize, options)
	assert.NoError(t, err)

	img, err := OpenImage(ioctx, name, NoSnapshot)
	assert.NoError(t, err)
	defer func() {
		assert.NoError(t, img.Close())
		assert.NoError(t, img.Remove())
	}()

	type diResult struct {
		offset uint64
		length uint64
	}
	calls := []diResult{}

	_, err = img.WriteAt([]byte("sometimes you feel like a nut"), 0)
	assert.NoError(t, err)

	err = img.DiffIterateByID(
		DiffIterateByIDConfig{
			Offset: 0,
			Length: isize,
			Callback: func(o, l uint64, _ int, x interface{}) int {
				if v, ok := x.(int); ok {
					assert.EqualValues(t, 77, v)
				} else {
					t.Fatalf("incorrect type")
				}
				calls = append(calls, diResult{offset: o, length: l})
				return 0
			},
			Data: 77,
		})
	assert.NoError(t, err)
	if assert.Len(t, calls, 1) {
		assert.EqualValues(t, 0, calls[0].offset)
		assert.EqualValues(t, 29, calls[0].length)
	}

	calls = []diResult{}
	err = img.DiffIterateByID(
		DiffIterateByIDConfig{
			Offset: 0,
			Length: isize,
			Callback: func(o, l uint64, _ int, x interface{}) int {
				if v, ok := x.(string); ok {
					assert.EqualValues(t, "bob", v)
				} else {
					t.Fatalf("incorrect type")
				}
				calls = append(calls, diResult{offset: o, length: l})
				return 0
			},
			Data: "bob",
		})
	assert.NoError(t, err)
	if assert.Len(t, calls, 1) {
		assert.EqualValues(t, 0, calls[0].offset)
		assert.EqualValues(t, 29, calls[0].length)
	}
}
