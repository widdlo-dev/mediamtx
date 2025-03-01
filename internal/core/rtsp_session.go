package core

import (
	"encoding/hex"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/bluenviron/gortsplib/v3"
	"github.com/bluenviron/gortsplib/v3/pkg/auth"
	"github.com/bluenviron/gortsplib/v3/pkg/base"
	"github.com/bluenviron/gortsplib/v3/pkg/formats"
	"github.com/bluenviron/gortsplib/v3/pkg/media"
	"github.com/bluenviron/gortsplib/v3/pkg/url"
	"github.com/google/uuid"
	"github.com/pion/rtp"

	"github.com/aler9/mediamtx/internal/conf"
	"github.com/aler9/mediamtx/internal/externalcmd"
	"github.com/aler9/mediamtx/internal/formatprocessor"
	"github.com/aler9/mediamtx/internal/logger"
)

type rtspWriteFunc func(*rtp.Packet)

func getRTSPWriteFunc(medi *media.Media, forma formats.Format, stream *stream) rtspWriteFunc {
	switch forma.(type) {
	case *formats.H264:
		return func(pkt *rtp.Packet) {
			stream.writeUnit(medi, forma, &formatprocessor.UnitH264{
				RTPPackets: []*rtp.Packet{pkt},
				NTP:        time.Now(),
			})
		}

	case *formats.H265:
		return func(pkt *rtp.Packet) {
			stream.writeUnit(medi, forma, &formatprocessor.UnitH265{
				RTPPackets: []*rtp.Packet{pkt},
				NTP:        time.Now(),
			})
		}

	case *formats.VP8:
		return func(pkt *rtp.Packet) {
			stream.writeUnit(medi, forma, &formatprocessor.UnitVP8{
				RTPPackets: []*rtp.Packet{pkt},
				NTP:        time.Now(),
			})
		}

	case *formats.VP9:
		return func(pkt *rtp.Packet) {
			stream.writeUnit(medi, forma, &formatprocessor.UnitVP9{
				RTPPackets: []*rtp.Packet{pkt},
				NTP:        time.Now(),
			})
		}

	case *formats.MPEG2Audio:
		return func(pkt *rtp.Packet) {
			stream.writeUnit(medi, forma, &formatprocessor.UnitMPEG2Audio{
				RTPPackets: []*rtp.Packet{pkt},
				NTP:        time.Now(),
			})
		}

	case *formats.MPEG4Audio:
		return func(pkt *rtp.Packet) {
			stream.writeUnit(medi, forma, &formatprocessor.UnitMPEG4Audio{
				RTPPackets: []*rtp.Packet{pkt},
				NTP:        time.Now(),
			})
		}

	case *formats.Opus:
		return func(pkt *rtp.Packet) {
			stream.writeUnit(medi, forma, &formatprocessor.UnitOpus{
				RTPPackets: []*rtp.Packet{pkt},
				NTP:        time.Now(),
			})
		}

	default:
		return func(pkt *rtp.Packet) {
			stream.writeUnit(medi, forma, &formatprocessor.UnitGeneric{
				RTPPackets: []*rtp.Packet{pkt},
				NTP:        time.Now(),
			})
		}
	}
}

type rtspSessionPathManager interface {
	publisherAdd(req pathPublisherAddReq) pathPublisherAnnounceRes
	readerAdd(req pathReaderAddReq) pathReaderSetupPlayRes
}

type rtspSessionParent interface {
	logger.Writer
}

type rtspSession struct {
	isTLS           bool
	protocols       map[conf.Protocol]struct{}
	session         *gortsplib.ServerSession
	author          *gortsplib.ServerConn
	externalCmdPool *externalcmd.Pool
	pathManager     rtspSessionPathManager
	parent          rtspSessionParent

	uuid       uuid.UUID
	created    time.Time
	path       *path
	stream     *stream
	state      gortsplib.ServerSessionState
	stateMutex sync.Mutex
	onReadCmd  *externalcmd.Cmd // read
}

func newRTSPSession(
	isTLS bool,
	protocols map[conf.Protocol]struct{},
	session *gortsplib.ServerSession,
	sc *gortsplib.ServerConn,
	externalCmdPool *externalcmd.Pool,
	pathManager rtspSessionPathManager,
	parent rtspSessionParent,
) *rtspSession {
	s := &rtspSession{
		isTLS:           isTLS,
		protocols:       protocols,
		session:         session,
		author:          sc,
		externalCmdPool: externalCmdPool,
		pathManager:     pathManager,
		parent:          parent,
		uuid:            uuid.New(),
		created:         time.Now(),
	}

	s.Log(logger.Info, "created by %v", s.author.NetConn().RemoteAddr())

	return s
}

// Close closes a Session.
func (s *rtspSession) close() {
	s.session.Close()
}

func (s *rtspSession) safeState() gortsplib.ServerSessionState {
	s.stateMutex.Lock()
	defer s.stateMutex.Unlock()
	return s.state
}

func (s *rtspSession) remoteAddr() net.Addr {
	return s.author.NetConn().RemoteAddr()
}

func (s *rtspSession) Log(level logger.Level, format string, args ...interface{}) {
	id := hex.EncodeToString(s.uuid[:4])
	s.parent.Log(level, "[session %s] "+format, append([]interface{}{id}, args...)...)
}

// onClose is called by rtspServer.
func (s *rtspSession) onClose(err error) {
	if s.session.State() == gortsplib.ServerSessionStatePlay {
		if s.onReadCmd != nil {
			s.onReadCmd.Close()
			s.onReadCmd = nil
			s.Log(logger.Info, "runOnRead command stopped")
		}
	}

	switch s.session.State() {
	case gortsplib.ServerSessionStatePrePlay, gortsplib.ServerSessionStatePlay:
		s.path.readerRemove(pathReaderRemoveReq{author: s})

	case gortsplib.ServerSessionStatePreRecord, gortsplib.ServerSessionStateRecord:
		s.path.publisherRemove(pathPublisherRemoveReq{author: s})
	}

	s.path = nil
	s.stream = nil

	s.Log(logger.Info, "destroyed (%v)", err)
}

// onAnnounce is called by rtspServer.
func (s *rtspSession) onAnnounce(c *rtspConn, ctx *gortsplib.ServerHandlerOnAnnounceCtx) (*base.Response, error) {
	if len(ctx.Path) == 0 || ctx.Path[0] != '/' {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, fmt.Errorf("invalid path")
	}
	ctx.Path = ctx.Path[1:]

	if c.authNonce == "" {
		c.authNonce = auth.GenerateNonce()
	}

	res := s.pathManager.publisherAdd(pathPublisherAddReq{
		author:   s,
		pathName: ctx.Path,
		credentials: authCredentials{
			query:       ctx.Query,
			ip:          c.ip(),
			proto:       authProtocolRTSP,
			id:          &c.uuid,
			rtspRequest: ctx.Request,
			rtspBaseURL: nil,
			rtspNonce:   c.authNonce,
		},
	})

	if res.err != nil {
		switch terr := res.err.(type) {
		case pathErrAuth:
			return c.handleAuthError(terr.wrapped)

		default:
			return &base.Response{
				StatusCode: base.StatusBadRequest,
			}, res.err
		}
	}

	s.path = res.path

	s.stateMutex.Lock()
	s.state = gortsplib.ServerSessionStatePreRecord
	s.stateMutex.Unlock()

	return &base.Response{
		StatusCode: base.StatusOK,
	}, nil
}

// onSetup is called by rtspServer.
func (s *rtspSession) onSetup(c *rtspConn, ctx *gortsplib.ServerHandlerOnSetupCtx,
) (*base.Response, *gortsplib.ServerStream, error) {
	if len(ctx.Path) == 0 || ctx.Path[0] != '/' {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, nil, fmt.Errorf("invalid path")
	}
	ctx.Path = ctx.Path[1:]

	// in case the client is setupping a stream with UDP or UDP-multicast, and these
	// transport protocols are disabled, gortsplib already blocks the request.
	// we have only to handle the case in which the transport protocol is TCP
	// and it is disabled.
	if ctx.Transport == gortsplib.TransportTCP {
		if _, ok := s.protocols[conf.Protocol(gortsplib.TransportTCP)]; !ok {
			return &base.Response{
				StatusCode: base.StatusUnsupportedTransport,
			}, nil, nil
		}
	}

	switch s.session.State() {
	case gortsplib.ServerSessionStateInitial, gortsplib.ServerSessionStatePrePlay: // play
		baseURL := &url.URL{
			Scheme:   ctx.Request.URL.Scheme,
			Host:     ctx.Request.URL.Host,
			Path:     ctx.Path,
			RawQuery: ctx.Query,
		}

		if ctx.Query != "" {
			baseURL.RawQuery += "/"
		} else {
			baseURL.Path += "/"
		}

		if c.authNonce == "" {
			c.authNonce = auth.GenerateNonce()
		}

		res := s.pathManager.readerAdd(pathReaderAddReq{
			author:   s,
			pathName: ctx.Path,
			credentials: authCredentials{
				query:       ctx.Query,
				ip:          c.ip(),
				proto:       authProtocolRTSP,
				id:          &c.uuid,
				rtspRequest: ctx.Request,
				rtspBaseURL: baseURL,
				rtspNonce:   c.authNonce,
			},
		})

		if res.err != nil {
			switch terr := res.err.(type) {
			case pathErrAuth:
				res, err := c.handleAuthError(terr.wrapped)
				return res, nil, err

			case pathErrNoOnePublishing:
				return &base.Response{
					StatusCode: base.StatusNotFound,
				}, nil, res.err

			default:
				return &base.Response{
					StatusCode: base.StatusBadRequest,
				}, nil, res.err
			}
		}

		s.path = res.path
		s.stream = res.stream

		s.stateMutex.Lock()
		s.state = gortsplib.ServerSessionStatePrePlay
		s.stateMutex.Unlock()

		return &base.Response{
			StatusCode: base.StatusOK,
		}, res.stream.rtspStream, nil

	default: // record
		return &base.Response{
			StatusCode: base.StatusOK,
		}, nil, nil
	}
}

// onPlay is called by rtspServer.
func (s *rtspSession) onPlay(ctx *gortsplib.ServerHandlerOnPlayCtx) (*base.Response, error) {
	h := make(base.Header)

	if s.session.State() == gortsplib.ServerSessionStatePrePlay {
		s.Log(logger.Info, "is reading from path '%s', with %s, %s",
			s.path.name,
			s.session.SetuppedTransport(),
			sourceMediaInfo(s.session.SetuppedMedias()))

		pathConf := s.path.safeConf()

		if pathConf.RunOnRead != "" {
			s.Log(logger.Info, "runOnRead command started")
			s.onReadCmd = externalcmd.NewCmd(
				s.externalCmdPool,
				pathConf.RunOnRead,
				pathConf.RunOnReadRestart,
				s.path.externalCmdEnv(),
				func(co int) {
					s.Log(logger.Info, "runOnRead command exited with code %d", co)
				})
		}

		s.stateMutex.Lock()
		s.state = gortsplib.ServerSessionStatePlay
		s.stateMutex.Unlock()
	}

	return &base.Response{
		StatusCode: base.StatusOK,
		Header:     h,
	}, nil
}

// onRecord is called by rtspServer.
func (s *rtspSession) onRecord(ctx *gortsplib.ServerHandlerOnRecordCtx) (*base.Response, error) {
	res := s.path.publisherStart(pathPublisherStartReq{
		author:             s,
		medias:             s.session.AnnouncedMedias(),
		generateRTPPackets: false,
	})
	if res.err != nil {
		return &base.Response{
			StatusCode: base.StatusBadRequest,
		}, res.err
	}

	s.Log(logger.Info, "is publishing to path '%s', with %s, %s",
		s.path.name,
		s.session.SetuppedTransport(),
		sourceMediaInfo(s.session.AnnouncedMedias()))

	s.stream = res.stream

	for _, medi := range s.session.AnnouncedMedias() {
		for _, forma := range medi.Formats {
			writeFunc := getRTSPWriteFunc(medi, forma, s.stream)

			ctx.Session.OnPacketRTP(medi, forma, func(pkt *rtp.Packet) {
				writeFunc(pkt)
			})
		}
	}

	s.stateMutex.Lock()
	s.state = gortsplib.ServerSessionStateRecord
	s.stateMutex.Unlock()

	return &base.Response{
		StatusCode: base.StatusOK,
	}, nil
}

// onPause is called by rtspServer.
func (s *rtspSession) onPause(ctx *gortsplib.ServerHandlerOnPauseCtx) (*base.Response, error) {
	switch s.session.State() {
	case gortsplib.ServerSessionStatePlay:
		if s.onReadCmd != nil {
			s.Log(logger.Info, "runOnRead command stopped")
			s.onReadCmd.Close()
		}

		s.stateMutex.Lock()
		s.state = gortsplib.ServerSessionStatePrePlay
		s.stateMutex.Unlock()

	case gortsplib.ServerSessionStateRecord:
		s.path.publisherStop(pathPublisherStopReq{author: s})

		s.stateMutex.Lock()
		s.state = gortsplib.ServerSessionStatePreRecord
		s.stateMutex.Unlock()
	}

	return &base.Response{
		StatusCode: base.StatusOK,
	}, nil
}

// apiReaderDescribe implements reader.
func (s *rtspSession) apiReaderDescribe() interface{} {
	var typ string
	if s.isTLS {
		typ = "rtspsSession"
	} else {
		typ = "rtspSession"
	}

	return struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}{typ, s.uuid.String()}
}

// apiSourceDescribe implements source.
func (s *rtspSession) apiSourceDescribe() interface{} {
	var typ string
	if s.isTLS {
		typ = "rtspsSession"
	} else {
		typ = "rtspSession"
	}

	return struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}{typ, s.uuid.String()}
}

// onPacketLost is called by rtspServer.
func (s *rtspSession) onPacketLost(ctx *gortsplib.ServerHandlerOnPacketLostCtx) {
	s.Log(logger.Warn, ctx.Error.Error())
}

// onDecodeError is called by rtspServer.
func (s *rtspSession) onDecodeError(ctx *gortsplib.ServerHandlerOnDecodeErrorCtx) {
	s.Log(logger.Warn, ctx.Error.Error())
}
