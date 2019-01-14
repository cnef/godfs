package libclient

import (
	"app"
	"container/list"
	"encoding/json"
	"errors"
	"io"
	"libcommon"
	"libcommon/bridge"
	"math/rand"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"util/file"
	"util/logger"
	"crypto/md5"
	"libcommon/bridgev2"
	"util/pool"
)

// each client has one tcp connection with storage server,
// once the connection is broken, the client will destroy.
// one client can only do 1 operation at a time.
var addLock *sync.Mutex
var NO_TRACKER_ERROR = errors.New("no tracker server available")
var NO_STORAGE_ERROR = errors.New("no storage server available")

func init() {
	addLock = new(sync.Mutex)
}

// Client have different meanings under different use cases.
// client usually communicate with tracker server.
type Client struct {
	TrackerMaintainer *TrackerMaintainer // tracker maintainer for client
	connPool          *pool.ClientConnectionPool
	MaxConnPerServer  int // 客户端和每个服务建立的最大连接数，web项目中建议设置为和最大线程相同的数量
}

// create a new client.
func NewClient(MaxConnPerServer int) *Client {
	logger.Debug("init native godfs client, max conn per server:", MaxConnPerServer)
	connPool := &pool.ClientConnectionPool{}
	connPool.Init(MaxConnPerServer)
	return &Client{connPool: connPool}
}

// upload file to storage server.
func (client *Client) Upload(path string, group string, startTime time.Time, skipCheck bool) (string, error) {
	fi, e := file.GetFile(path)
	if e != nil {
		return "", errors.New("error upload file " + path + " due to " + e.Error())
	}
	defer fi.Close()

	fileMd5 := ""
	logger.Info("upload file:", fi.Name())

	if !skipCheck {
		logger.Debug("pre check file md5:", fi.Name())
		md5, ee := file.GetFileMd5(path)
		if ee == nil {
			fileMd5 = md5
			qfi, ee1 := client.QueryFile(md5)
			if qfi != nil {
				sm := "S"
				if qfi.PartNum > 1 {
					sm = "M"
				}
				logger.Debug("file already exists, skip upload.")
				return qfi.Group + "/" + qfi.Instance + "/" + sm + "/" + qfi.Md5, nil
			} else {
				logger.Debug("error query file info from tracker server:", ee1)
			}
		} else {
			logger.Debug("error check file md5:", ee, ", skip pre check.")
		}
	}

	var excludes list.List
	var member *app.StorageDO
	server := &app.ServerInfo{}
	var tcpClient *bridgev2.TcpBridgeClient

	for {
		// select a storage server which match the given regulation from all members
		member = selectStorageServer(group, "", &excludes, true)
		// no available storage server
		if member == nil {
			return "", NO_STORAGE_ERROR
		}
		// construct server info from storage member
		server.FromStorage(member)
		tcpClient = bridgev2.NewTcpClient(server)
		// connect to storage server
		e1 := tcpClient.Connect()
		if e1 != nil {
			h, p := server.GetHostAndPortByAccessFlag()
			logger.Error("error connect to storage server", h + ":" + strconv.Itoa(p), "due to:", e1.Error())
			excludes.PushBack(member)
			continue
		}
		// validate connection
		_, e2 := tcpClient.Validate()
		if e2 != nil {
			h, p := server.GetHostAndPortByAccessFlag()
			logger.Error("error validate with storage server", h + ":" + strconv.Itoa(p), "due to:", e2.Error())
			excludes.PushBack(member)
			continue
		}
		// connection and validate success, continue works below
		break
	}

	h, p := server.GetHostAndPortByAccessFlag()
	logger.Info("using storage server", h + ":" + strconv.Itoa(p), "(" + member.Uuid + ")")

	fInfo, _ := fi.Stat()
	uploadMeta := &bridgev2.UploadFileMeta{
		FileSize: fInfo.Size(),
		FileExt:  file.GetFileExt(fInfo.Name()),
		Md5:      fileMd5,
	}
	destroy := false
	resMeta, err := tcpClient.UploadFile(uploadMeta, func(manager *bridgev2.ConnectionManager, frame *bridgev2.Frame) error {
		// begin upload file body bytes
		buff, _ := bridgev2.MakeBytes(app.BUFF_SIZE, false, 0, false)
		var finish, total int64
		var stopFlag = false
		defer func() {
			stopFlag = true
			bridgev2.RecycleBytes(buff)
		}()
		total = fInfo.Size()
		finish = 0
		go libcommon.ShowPercent(&total, &finish, &stopFlag, startTime)
		for {
			len5, e4 := fi.Read(buff)
			if e4 != nil && e4 != io.EOF {
				return e4
			}
			if len5 > 0 {
				len3, e5 := manager.Conn.Write(buff[0:len5])
				finish += int64(len5)
				if e5 != nil {
					destroy = true
					return e5
				}
				if len3 != len(buff[0:len5]) {
					destroy = true
					return errors.New("could not write enough bytes")
				}
			} else {
				if e4 != io.EOF {
					return e4
				} else {
					logger.Debug("upload finish")
				}
				break
			}
		}
		return nil
	})

	if destroy {
		tcpClient.GetConnManager().Destroy()
	} else {
		tcpClient.GetConnManager().Close()
	}
	return resMeta.Path, err
}

// query file from tracker server.
func (client *Client) QueryFile(pathOrMd5 string) (*bridge.File, error) {
	logger.Debug("query file info:", pathOrMd5)
	var result *bridge.File
	for ele := client.TrackerMaintainer.TrackerInstances.Front(); ele != nil; ele = ele.Next() {
		queryMeta := &bridge.OperationQueryFileRequest{PathOrMd5: pathOrMd5}
		connBridge := ele.Value.(*TrackerInstance).connBridge
		e11 := connBridge.SendRequest(bridge.O_QUERY_FILE, queryMeta, 0, nil)
		if e11 != nil {
			connBridge.Close()
			continue
		}
		e12 := connBridge.ReceiveResponse(func(response *bridge.Meta, in io.Reader) error {
			if response.Err != nil {
				return response.Err
			}
			var queryResponse = &bridge.OperationQueryFileResponse{}
			e4 := json.Unmarshal(response.MetaBody, queryResponse)
			if e4 != nil {
				return e4
			}
			if queryResponse.Status != bridge.STATUS_OK && queryResponse.Status != bridge.STATUS_NOT_FOUND {
				return errors.New("error connect to server, server response status:" + strconv.Itoa(queryResponse.Status))
			}
			result = queryResponse.File
			return nil
		})
		if e12 != nil {
			connBridge.Close()
			continue
		}
		if result != nil {
			return result, nil
		}
	}
	return result, nil
}

func (client *Client) DownloadFile(path string, start int64, offset int64, writerHandler func(realPath string, fileLen uint64, reader io.Reader) error) error {
	path = strings.TrimSpace(path)
	if strings.Index(path, "/") != 0 {
		path = "/" + path
	}
	if mat, _ := regexp.Match(app.PATH_REGEX, []byte(path)); !mat {
		return errors.New("file path format error")
	}
	return download(path, start, offset, false, new(list.List), client, writerHandler)
}

func download(path string, start int64, offset int64, fromSrc bool, excludes *list.List, client *Client,
	writerHandler func(realPath string, fileLen uint64, reader io.Reader) error) error {
	downloadMeta := &bridge.OperationDownloadFileRequest{
		Path:   path,
		Start:  start,
		Offset: offset,
	}
	group := regexp.MustCompile(app.PATH_REGEX).ReplaceAllString(path, "${1}")
	instanceId := regexp.MustCompile(app.PATH_REGEX).ReplaceAllString(path, "${2}")

	var connBridge *bridge.Bridge
	var member *bridge.ExpireMember
	for {
		var mem *bridge.ExpireMember
		if fromSrc {
			mem = selectStorageServer(group, instanceId, excludes, false)
			if mem != nil {
				host, port := mem.GetHostAndPortByAccessFlag()
				logger.Debug("try to download file from source server:", host+":"+strconv.Itoa(port))
			}
		} else {
			mem = selectStorageServer(group, "", excludes, false)
		}
		if mem != nil {
			excludes.PushBack(mem)
		}
		// no available storage
		if mem == nil {
			if !fromSrc {
				return NO_STORAGE_ERROR
			} else {
				logger.Debug("source server is not available(" + instanceId + ")")
				fromSrc = false
				continue
			}
		}
		// TODO when download is busy and no connection available, shall skip current download task.
		host, port := mem.GetHostAndPortByAccessFlag()
		logger.Debug("using storage server:", host+":"+strconv.Itoa(port))
		cb, e12 := client.connPool.GetConnBridge(mem)
		if e12 != nil {
			logger.Error(e12)
			/*if e12 != MAX_CONN_EXCEED_ERROR {
			    if !srcInstanceFail {
			        srcInstanceFail = true
			    }
			}*/
			excludes.PushBack(mem)
			continue
		}
		connBridge = cb
		member = mem
		break
	}
	logger.Info("download from:", member.AdvertiseAddr+":"+strconv.Itoa(member.Port))

	e2 := connBridge.SendRequest(bridge.O_DOWNLOAD_FILE, downloadMeta, 0, nil)
	if e2 != nil {
		client.connPool.ReturnBrokenConnBridge(member, connBridge)
		// if download fail, try to download from other storage server
		return download(path, start, offset, false, excludes, client, writerHandler)
	}

	var responseCode = bridge.STATUS_INTERNAL_SERVER_ERROR
	// receive response
	e3 := connBridge.ReceiveResponse(func(response *bridge.Meta, in io.Reader) error {
		if response.Err != nil {
			return response.Err
		}
		var downloadResponse = &bridge.OperationDownloadFileResponse{}
		e4 := json.Unmarshal(response.MetaBody, downloadResponse)
		if e4 != nil {
			return e4
		}
		responseCode = downloadResponse.Status
		if downloadResponse.Status == bridge.STATUS_NOT_FOUND {
			return bridge.FILE_NOT_FOUND_ERROR
		}
		if downloadResponse.Status != bridge.STATUS_OK {
			logger.Error("error connect to server, server response status:" + strconv.Itoa(downloadResponse.Status))
			return bridge.DOWNLOAD_FILE_ERROR
		}
		return writerHandler(path, response.BodyLength, connBridge.GetConn())
	})
	if e3 != nil {
		if responseCode == bridge.STATUS_NOT_FOUND || responseCode == bridge.STATUS_OK {
			client.connPool.ReturnConnBridge(member, connBridge)
		} else {
			client.connPool.ReturnBrokenConnBridge(member, connBridge)
		}
		// if download fail, try to download from other storage server
		return download(path, start, offset, false, excludes, client, writerHandler)
	} else {
		client.connPool.ReturnConnBridge(member, connBridge)
	}
	return nil
}

// select a storage server matching given group and instanceId
// excludes contains fail storage and not gonna use this time.
func selectStorageServer(group string, instanceId string, excludes *list.List, upload bool) *app.StorageDO {
	memberIteLock.Lock()
	defer memberIteLock.Unlock()
	var pick list.List
	for ele := GroupMembers.Front(); ele != nil; ele = ele.Next() {
		b := ele.Value.(*app.StorageDO)
		if containsMember(b, excludes) || (upload && b.ReadOnly) {
			continue
		}
		match1 := false
		match2 := false
		if group == "" || group == b.Group {
			match1 = true
		}
		if instanceId == "" || instanceId == b.InstanceId {
			match2 = true
		}
		if match1 && match2 {
			pick.PushBack(b)
		}
	}
	if pick.Len() == 0 {
		return nil
	}
	rd := rand.Intn(pick.Len())
	index := 0
	for ele := pick.Front(); ele != nil; ele = ele.Next() {
		if index == rd {
			return ele.Value.(*app.StorageDO)
		}
		index++
	}
	return nil
}

// query if a list contains the given storage server.
func containsMember(mem *app.StorageDO, excludes *list.List) bool {
	if excludes == nil {
		return false
	}
	for ele := excludes.Front(); ele != nil; ele = ele.Next() {
		if ele.Value.(*app.StorageDO).Uuid == mem.Uuid {
			return true
		}
	}
	return false
}
