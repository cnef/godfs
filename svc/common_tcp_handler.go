package svc

import (
	"encoding/json"
	"github.com/hetianyi/godfs/api"
	"github.com/hetianyi/godfs/binlog"
	"github.com/hetianyi/godfs/common"
	"github.com/hetianyi/godfs/reg"
	"github.com/hetianyi/gox/convert"
	"github.com/hetianyi/gox/file"
	"github.com/hetianyi/gox/logger"
	"hash"
	"io"
	"os"
)

const MaxConnPerServer uint = 100

var (
	clientAPI             api.ClientAPI
	writableBinlogManager binlog.XBinlogManager
)

// DigestProxyWriter is a writer proxy which can calculate crc and md5 for the stream file.
type DigestProxyWriter struct {
	crcH hash.Hash32
	md5H hash.Hash
	out  io.Writer
}

func (w *DigestProxyWriter) Write(p []byte) (n int, err error) {
	n, err = w.crcH.Write(p)
	if err != nil {
		return n, err
	}
	n, err = w.md5H.Write(p)
	if err != nil {
		return n, err
	}
	return w.out.Write(p)
}

// InitializeClientAPI initializes client API.
func InitializeClientAPI(config *api.Config) {
	clientAPI = api.NewClient()
	clientAPI.SetConfig(config)
}

func authenticationHandler(header *common.Header, secret string) (*common.Header, *common.Instance, io.Reader, int64, error) {
	if header.Attributes == nil {
		return &common.Header{
			Result: common.UNAUTHORIZED,
			Msg:    "authentication failed",
		}, nil, nil, 0, nil
	}
	s := header.Attributes["secret"]
	if s != secret {
		return &common.Header{
			Result: common.UNAUTHORIZED,
			Msg:    "authentication failed",
		}, nil, nil, 0, nil
	}

	var instance *common.Instance
	if common.BootAs == common.BOOT_TRACKER {
		// parse instance info.
		s1 := header.Attributes["instance"]
		instance = &common.Instance{}
		if err := json.Unmarshal([]byte(s1), instance); err != nil {
			return &common.Header{
				Result: common.ERROR,
				Msg:    err.Error(),
			}, nil, nil, 0, err
		}
		if err := reg.Put(instance); err != nil {
			return &common.Header{
				Result: common.ERROR,
				Msg:    err.Error(),
			}, nil, nil, 0, err
		}
	}

	return &common.Header{
		Result: common.SUCCESS,
		Msg:    "authentication success",
	}, instance, nil, 0, nil
}

func updateFileReferenceCount(path string, value int64) error {
	oldFile, err := file.OpenFile(path, os.O_RDWR, 0666)
	if err != nil {
		return err
	}
	defer oldFile.Close()
	tailRefBytes := make([]byte, 8)
	if _, err := oldFile.Seek(-4, 2); err != nil {
		return err
	}
	if _, err := io.ReadAtLeast(oldFile, tailRefBytes[4:], 4); err != nil {
		return err
	}
	// must add lock
	count := convert.Bytes2Length(tailRefBytes)
	logger.Debug("file referenced count: ", count)
	count += value
	convert.Length2Bytes(count, tailRefBytes)
	if _, err := oldFile.Seek(-4, 2); err != nil {
		return err
	}
	if _, err := oldFile.Write(tailRefBytes[4:]); err != nil {
		return err
	}
	return nil
}