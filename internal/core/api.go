package core

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"reflect"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/aler9/mediamtx/internal/conf"
	"github.com/aler9/mediamtx/internal/logger"
)

func interfaceIsEmpty(i interface{}) bool {
	return reflect.ValueOf(i).Kind() != reflect.Ptr || reflect.ValueOf(i).IsNil()
}

func fillStruct(dest interface{}, source interface{}) {
	rvsource := reflect.ValueOf(source).Elem()
	rvdest := reflect.ValueOf(dest)
	nf := rvsource.NumField()
	for i := 0; i < nf; i++ {
		fnew := rvsource.Field(i)
		if !fnew.IsNil() {
			f := rvdest.Elem().FieldByName(rvsource.Type().Field(i).Name)
			if f.Kind() == reflect.Ptr {
				f.Set(fnew)
			} else {
				f.Set(fnew.Elem())
			}
		}
	}
}

func generateStructWithOptionalFields(model interface{}) interface{} {
	var fields []reflect.StructField

	rt := reflect.TypeOf(model)
	nf := rt.NumField()
	for i := 0; i < nf; i++ {
		f := rt.Field(i)
		j := f.Tag.Get("json")

		if j != "-" && j != "paths" {
			fields = append(fields, reflect.StructField{
				Name: f.Name,
				Type: reflect.PtrTo(f.Type),
				Tag:  f.Tag,
			})
		}
	}

	return reflect.New(reflect.StructOf(fields)).Interface()
}

func loadConfData(ctx *gin.Context) (interface{}, error) {
	in := generateStructWithOptionalFields(conf.Conf{})
	err := json.NewDecoder(ctx.Request.Body).Decode(in)
	if err != nil {
		return nil, err
	}

	return in, err
}

func loadConfPathData(ctx *gin.Context) (interface{}, error) {
	in := generateStructWithOptionalFields(conf.PathConf{})
	err := json.NewDecoder(ctx.Request.Body).Decode(in)
	if err != nil {
		return nil, err
	}

	return in, err
}

type apiPathManager interface {
	apiPathsList() pathAPIPathsListRes
}

type apiHLSServer interface {
	apiMuxersList() hlsServerAPIMuxersListRes
}

type apiRTSPServer interface {
	apiConnsList() rtspServerAPIConnsListRes
	apiSessionsList() rtspServerAPISessionsListRes
	apiSessionsKick(string) rtspServerAPISessionsKickRes
}

type apiRTMPServer interface {
	apiConnsList() rtmpServerAPIConnsListRes
	apiConnsKick(id string) rtmpServerAPIConnsKickRes
}

type apiParent interface {
	logger.Writer
	apiConfigSet(conf *conf.Conf)
}

type apiWebRTCServer interface {
	apiConnsList() webRTCServerAPIConnsListRes
	apiConnsKick(id string) webRTCServerAPIConnsKickRes
}

type api struct {
	conf         *conf.Conf
	pathManager  apiPathManager
	rtspServer   apiRTSPServer
	rtspsServer  apiRTSPServer
	rtmpServer   apiRTMPServer
	rtmpsServer  apiRTMPServer
	hlsServer    apiHLSServer
	webRTCServer apiWebRTCServer
	parent       apiParent

	ln         net.Listener
	httpServer *http.Server
	mutex      sync.Mutex
}

func newAPI(
	address string,
	readTimeout conf.StringDuration,
	conf *conf.Conf,
	pathManager apiPathManager,
	rtspServer apiRTSPServer,
	rtspsServer apiRTSPServer,
	rtmpServer apiRTMPServer,
	rtmpsServer apiRTMPServer,
	hlsServer apiHLSServer,
	webRTCServer apiWebRTCServer,
	parent apiParent,
) (*api, error) {
	ln, err := net.Listen(restrictNetwork("tcp", address))
	if err != nil {
		return nil, err
	}

	a := &api{
		conf:         conf,
		pathManager:  pathManager,
		rtspServer:   rtspServer,
		rtspsServer:  rtspsServer,
		rtmpServer:   rtmpServer,
		rtmpsServer:  rtmpsServer,
		hlsServer:    hlsServer,
		webRTCServer: webRTCServer,
		parent:       parent,
		ln:           ln,
	}

	router := gin.New()
	router.SetTrustedProxies(nil)

	mwLog := httpLoggerMiddleware(a)
	router.NoRoute(mwLog, httpServerHeaderMiddleware)
	group := router.Group("/", mwLog, httpServerHeaderMiddleware)

	group.GET("/v1/config/get", a.onConfigGet)
	group.POST("/v1/config/set", a.onConfigSet)
	group.POST("/v1/config/paths/add/*name", a.onConfigPathsAdd)
	group.POST("/v1/config/paths/edit/*name", a.onConfigPathsEdit)
	group.POST("/v1/config/paths/remove/*name", a.onConfigPathsDelete)

	if !interfaceIsEmpty(a.hlsServer) {
		group.GET("/v1/hlsmuxers/list", a.onHLSMuxersList)
	}

	group.GET("/v1/paths/list", a.onPathsList)

	if !interfaceIsEmpty(a.rtspServer) {
		group.GET("/v1/rtspconns/list", a.onRTSPConnsList)
		group.GET("/v1/rtspsessions/list", a.onRTSPSessionsList)
		group.POST("/v1/rtspsessions/kick/:id", a.onRTSPSessionsKick)
	}

	if !interfaceIsEmpty(a.rtspsServer) {
		group.GET("/v1/rtspsconns/list", a.onRTSPSConnsList)
		group.GET("/v1/rtspssessions/list", a.onRTSPSSessionsList)
		group.POST("/v1/rtspssessions/kick/:id", a.onRTSPSSessionsKick)
	}

	if !interfaceIsEmpty(a.rtmpServer) {
		group.GET("/v1/rtmpconns/list", a.onRTMPConnsList)
		group.POST("/v1/rtmpconns/kick/:id", a.onRTMPConnsKick)
	}

	if !interfaceIsEmpty(a.rtmpsServer) {
		group.GET("/v1/rtmpsconns/list", a.onRTMPSConnsList)
		group.POST("/v1/rtmpsconns/kick/:id", a.onRTMPSConnsKick)
	}

	if !interfaceIsEmpty(a.webRTCServer) {
		group.GET("/v1/webrtcconns/list", a.onWebRTCConnsList)
		group.POST("/v1/webrtcconns/kick/:id", a.onWebRTCConnsKick)
	}

	a.httpServer = &http.Server{
		Handler:           router,
		ReadHeaderTimeout: time.Duration(readTimeout),
		ErrorLog:          log.New(&nilWriter{}, "", 0),
	}

	go a.httpServer.Serve(ln)

	a.Log(logger.Info, "listener opened on "+address)

	return a, nil
}

func (a *api) close() {
	a.Log(logger.Info, "listener is closing")
	a.httpServer.Shutdown(context.Background())
	a.ln.Close() // in case Shutdown() is called before Serve()
}

func (a *api) Log(level logger.Level, format string, args ...interface{}) {
	a.parent.Log(level, "[API] "+format, args...)
}

func (a *api) onConfigGet(ctx *gin.Context) {
	a.mutex.Lock()
	c := a.conf
	a.mutex.Unlock()

	ctx.JSON(http.StatusOK, c)
}

func (a *api) onConfigSet(ctx *gin.Context) {
	in, err := loadConfData(ctx)
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	a.mutex.Lock()
	defer a.mutex.Unlock()

	newConf := a.conf.Clone()

	fillStruct(newConf, in)

	err = newConf.Check()
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	a.conf = newConf

	// since reloading the configuration can cause the shutdown of the API,
	// call it in a goroutine
	go a.parent.apiConfigSet(newConf)

	ctx.Status(http.StatusOK)
}

func (a *api) onConfigPathsAdd(ctx *gin.Context) {
	name := ctx.Param("name")
	if len(name) < 2 || name[0] != '/' {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}
	name = name[1:]

	in, err := loadConfPathData(ctx)
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	a.mutex.Lock()
	defer a.mutex.Unlock()

	newConf := a.conf.Clone()

	if _, ok := newConf.Paths[name]; ok {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	newConfPath := &conf.PathConf{}
	fillStruct(newConfPath, in)

	newConf.Paths[name] = newConfPath

	err = newConf.Check()
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	a.conf = newConf

	// since reloading the configuration can cause the shutdown of the API,
	// call it in a goroutine
	go a.parent.apiConfigSet(newConf)

	ctx.Status(http.StatusOK)
}

func (a *api) onConfigPathsEdit(ctx *gin.Context) {
	name := ctx.Param("name")
	if len(name) < 2 || name[0] != '/' {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}
	name = name[1:]

	in, err := loadConfPathData(ctx)
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	a.mutex.Lock()
	defer a.mutex.Unlock()

	newConf := a.conf.Clone()

	newConfPath, ok := newConf.Paths[name]
	if !ok {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	fillStruct(newConfPath, in)

	err = newConf.Check()
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	a.conf = newConf

	// since reloading the configuration can cause the shutdown of the API,
	// call it in a goroutine
	go a.parent.apiConfigSet(newConf)

	ctx.Status(http.StatusOK)
}

func (a *api) onConfigPathsDelete(ctx *gin.Context) {
	name := ctx.Param("name")
	if len(name) < 2 || name[0] != '/' {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}
	name = name[1:]

	a.mutex.Lock()
	defer a.mutex.Unlock()

	newConf := a.conf.Clone()

	if _, ok := newConf.Paths[name]; !ok {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	delete(newConf.Paths, name)

	err := newConf.Check()
	if err != nil {
		ctx.AbortWithStatus(http.StatusBadRequest)
		return
	}

	a.conf = newConf

	// since reloading the configuration can cause the shutdown of the API,
	// call it in a goroutine
	go a.parent.apiConfigSet(newConf)

	ctx.Status(http.StatusOK)
}

func (a *api) onPathsList(ctx *gin.Context) {
	res := a.pathManager.apiPathsList()
	if res.err != nil {
		ctx.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	ctx.JSON(http.StatusOK, res.data)
}

func (a *api) onRTSPConnsList(ctx *gin.Context) {
	res := a.rtspServer.apiConnsList()
	if res.err != nil {
		ctx.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	ctx.JSON(http.StatusOK, res.data)
}

func (a *api) onRTSPSessionsList(ctx *gin.Context) {
	res := a.rtspServer.apiSessionsList()
	if res.err != nil {
		ctx.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	ctx.JSON(http.StatusOK, res.data)
}

func (a *api) onRTSPSessionsKick(ctx *gin.Context) {
	id := ctx.Param("id")

	res := a.rtspServer.apiSessionsKick(id)
	if res.err != nil {
		return
	}

	ctx.Status(http.StatusOK)
}

func (a *api) onRTSPSConnsList(ctx *gin.Context) {
	res := a.rtspsServer.apiConnsList()
	if res.err != nil {
		ctx.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	ctx.JSON(http.StatusOK, res.data)
}

func (a *api) onRTSPSSessionsList(ctx *gin.Context) {
	res := a.rtspsServer.apiSessionsList()
	if res.err != nil {
		ctx.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	ctx.JSON(http.StatusOK, res.data)
}

func (a *api) onRTSPSSessionsKick(ctx *gin.Context) {
	id := ctx.Param("id")

	res := a.rtspsServer.apiSessionsKick(id)
	if res.err != nil {
		return
	}

	ctx.Status(http.StatusOK)
}

func (a *api) onRTMPConnsList(ctx *gin.Context) {
	res := a.rtmpServer.apiConnsList()
	if res.err != nil {
		ctx.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	ctx.JSON(http.StatusOK, res.data)
}

func (a *api) onRTMPConnsKick(ctx *gin.Context) {
	id := ctx.Param("id")

	res := a.rtmpServer.apiConnsKick(id)
	if res.err != nil {
		return
	}

	ctx.Status(http.StatusOK)
}

func (a *api) onRTMPSConnsList(ctx *gin.Context) {
	res := a.rtmpsServer.apiConnsList()
	if res.err != nil {
		ctx.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	ctx.JSON(http.StatusOK, res.data)
}

func (a *api) onRTMPSConnsKick(ctx *gin.Context) {
	id := ctx.Param("id")

	res := a.rtmpsServer.apiConnsKick(id)
	if res.err != nil {
		return
	}

	ctx.Status(http.StatusOK)
}

func (a *api) onHLSMuxersList(ctx *gin.Context) {
	res := a.hlsServer.apiMuxersList()
	if res.err != nil {
		ctx.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	ctx.JSON(http.StatusOK, res.data)
}

func (a *api) onWebRTCConnsList(ctx *gin.Context) {
	res := a.webRTCServer.apiConnsList()
	if res.err != nil {
		ctx.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	ctx.JSON(http.StatusOK, res.data)
}

func (a *api) onWebRTCConnsKick(ctx *gin.Context) {
	id := ctx.Param("id")

	res := a.webRTCServer.apiConnsKick(id)
	if res.err != nil {
		return
	}

	ctx.Status(http.StatusOK)
}

// confReload is called by core.
func (a *api) confReload(conf *conf.Conf) {
	a.mutex.Lock()
	defer a.mutex.Unlock()
	a.conf = conf
}
