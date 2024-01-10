package box

import (
	"context"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"time"

	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing-box/common/taskmonitor"
	C "github.com/sagernet/sing-box/constant"
	"github.com/sagernet/sing-box/experimental"
	"github.com/sagernet/sing-box/experimental/cachefile"
	"github.com/sagernet/sing-box/experimental/libbox/platform"
	"github.com/sagernet/sing-box/inbound"
	"github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/outbound"
	"github.com/sagernet/sing-box/proxyprovider"
	"github.com/sagernet/sing-box/route"
	"github.com/sagernet/sing-box/script"
	"github.com/sagernet/sing/common"
	E "github.com/sagernet/sing/common/exceptions"
	F "github.com/sagernet/sing/common/format"
	"github.com/sagernet/sing/service"
	"github.com/sagernet/sing/service/pause"
)

var _ adapter.Service = (*Box)(nil)

type Box struct {
	createdAt      time.Time
	router         adapter.Router
	inbounds       []adapter.Inbound
	outbounds      []adapter.Outbound
	proxyProviders []adapter.ProxyProvider
	scripts        []*script.Script
	logFactory     log.Factory
	logger         log.ContextLogger
	preServices1   map[string]adapter.Service
	preServices2   map[string]adapter.Service
	postServices   map[string]adapter.Service
	reloadChan     chan struct{}
	done           chan struct{}
}

type Options struct {
	option.Options
	Context           context.Context
	PlatformInterface platform.Interface
	PlatformLogWriter log.PlatformWriter
}

func New(options Options) (*Box, error) {
	createdAt := time.Now()
	ctx := options.Context
	if ctx == nil {
		ctx = context.Background()
	}
	ctx = service.ContextWithDefaultRegistry(ctx)
	ctx = pause.ContextWithDefaultManager(ctx)
	reloadChan := make(chan struct{}, 1)
	experimentalOptions := common.PtrValueOrDefault(options.Experimental)
	applyDebugOptions(common.PtrValueOrDefault(experimentalOptions.Debug))
	var needCacheFile bool
	var needClashAPI bool
	var needV2RayAPI bool
	if experimentalOptions.CacheFile != nil && experimentalOptions.CacheFile.Enabled || options.PlatformLogWriter != nil {
		needCacheFile = true
	}
	if experimentalOptions.ClashAPI != nil || options.PlatformLogWriter != nil {
		needClashAPI = true
	}
	if experimentalOptions.V2RayAPI != nil && experimentalOptions.V2RayAPI.Listen != "" {
		needV2RayAPI = true
	}
	var defaultLogWriter io.Writer
	if options.PlatformInterface != nil {
		defaultLogWriter = io.Discard
	}
	logFactory, err := log.New(log.Options{
		Context:        ctx,
		Options:        common.PtrValueOrDefault(options.Log),
		Observable:     needClashAPI,
		DefaultWriter:  defaultLogWriter,
		BaseTime:       createdAt,
		PlatformWriter: options.PlatformLogWriter,
	})
	if err != nil {
		return nil, E.Cause(err, "create log factory")
	}
	routeOptions := common.PtrValueOrDefault(options.Route)
	dnsOptions := common.PtrValueOrDefault(options.DNS)
	var scripts []*script.Script
	for i, scriptOptions := range options.Scripts {
		var tag string
		if scriptOptions.Tag != "" {
			tag = scriptOptions.Tag
		} else {
			tag = F.ToString(i)
		}
		s, err := script.NewScript(
			ctx,
			logFactory.NewLogger(F.ToString("script", "[", tag, "]")),
			tag,
			scriptOptions,
		)
		if err != nil {
			return nil, E.Cause(err, "parse script[", i, "]")
		}
		scripts = append(scripts, s)
	}
	router, err := route.NewRouter(
		ctx,
		logFactory,
		routeOptions,
		dnsOptions,
		common.PtrValueOrDefault(options.NTP),
		options.Inbounds,
		options.PlatformInterface,
		reloadChan,
	)
	if err != nil {
		return nil, E.Cause(err, "parse route options")
	}
	inbounds := make([]adapter.Inbound, 0, len(options.Inbounds))
	outbounds := make([]adapter.Outbound, 0, len(options.Outbounds))
	for i, inboundOptions := range options.Inbounds {
		var in adapter.Inbound
		var tag string
		if inboundOptions.Tag != "" {
			tag = inboundOptions.Tag
		} else {
			tag = F.ToString(i)
		}
		in, err = inbound.New(
			ctx,
			router,
			logFactory.NewLogger(F.ToString("inbound/", inboundOptions.Type, "[", tag, "]")),
			inboundOptions,
			options.PlatformInterface,
		)
		if err != nil {
			return nil, E.Cause(err, "parse inbound[", i, "]")
		}
		inbounds = append(inbounds, in)
	}
	for i, outboundOptions := range options.Outbounds {
		var out adapter.Outbound
		var tag string
		if outboundOptions.Tag != "" {
			tag = outboundOptions.Tag
		} else {
			tag = F.ToString(i)
		}
		out, err = outbound.New(
			ctx,
			router,
			logFactory.NewLogger(F.ToString("outbound/", outboundOptions.Type, "[", tag, "]")),
			tag,
			outboundOptions)
		if err != nil {
			return nil, E.Cause(err, "parse outbound[", i, "]")
		}
		outbounds = append(outbounds, out)
	}
	var proxyProviders []adapter.ProxyProvider
	if len(options.ProxyProviders) > 0 {
		proxyProviders = make([]adapter.ProxyProvider, 0, len(options.ProxyProviders))
		for i, proxyProviderOptions := range options.ProxyProviders {
			var pp adapter.ProxyProvider
			var tag string
			if proxyProviderOptions.Tag != "" {
				tag = proxyProviderOptions.Tag
			} else {
				tag = F.ToString(i)
				proxyProviderOptions.Tag = tag
			}
			pp, err = proxyprovider.NewProxyProvider(ctx, router, logFactory.NewLogger(F.ToString("proxyprovider[", tag, "]")), tag, proxyProviderOptions)
			if err != nil {
				return nil, E.Cause(err, "parse proxyprovider[", i, "]")
			}
			outboundOptions, err := pp.StartGetOutbounds()
			if err != nil {
				return nil, E.Cause(err, "get outbounds from proxyprovider[", i, "]")
			}
			for i, outboundOptions := range outboundOptions {
				var out adapter.Outbound
				tag := outboundOptions.Tag
				out, err = outbound.New(
					ctx,
					router,
					logFactory.NewLogger(F.ToString("outbound/", outboundOptions.Type, "[", tag, "]")),
					tag,
					outboundOptions)
				if err != nil {
					return nil, E.Cause(err, "parse proxyprovider ["+pp.Tag()+"] outbound[", i, "]")
				}
				outbounds = append(outbounds, out)
			}
			proxyProviders = append(proxyProviders, pp)
		}
	}
	err = router.Initialize(inbounds, outbounds, func() adapter.Outbound {
		out, oErr := outbound.New(ctx, router, logFactory.NewLogger("outbound/direct"), "direct", option.Outbound{Type: "direct", Tag: "default"})
		common.Must(oErr)
		outbounds = append(outbounds, out)
		return out
	}, proxyProviders)
	if err != nil {
		return nil, err
	}
	if options.PlatformInterface != nil {
		err = options.PlatformInterface.Initialize(ctx, router)
		if err != nil {
			return nil, E.Cause(err, "initialize platform interface")
		}
	}
	preServices1 := make(map[string]adapter.Service)
	preServices2 := make(map[string]adapter.Service)
	postServices := make(map[string]adapter.Service)
	if needCacheFile {
		cacheFile := cachefile.New(ctx, common.PtrValueOrDefault(experimentalOptions.CacheFile))
		preServices1["cache file"] = cacheFile
		service.MustRegister[adapter.CacheFile](ctx, cacheFile)
	}
	if needClashAPI {
		clashAPIOptions := common.PtrValueOrDefault(experimentalOptions.ClashAPI)
		clashAPIOptions.ModeList = experimental.CalculateClashModeList(options.Options)
		clashServer, err := experimental.NewClashServer(ctx, router, logFactory.(log.ObservableFactory), clashAPIOptions)
		if err != nil {
			return nil, E.Cause(err, "create clash api server")
		}
		router.SetClashServer(clashServer)
		preServices2["clash api"] = clashServer
	}
	if needV2RayAPI {
		v2rayServer, err := experimental.NewV2RayServer(logFactory.NewLogger("v2ray-api"), common.PtrValueOrDefault(experimentalOptions.V2RayAPI))
		if err != nil {
			return nil, E.Cause(err, "create v2ray api server")
		}
		router.SetV2RayServer(v2rayServer)
		preServices2["v2ray api"] = v2rayServer
	}
	return &Box{
		router:         router,
		inbounds:       inbounds,
		outbounds:      outbounds,
		proxyProviders: proxyProviders,
		scripts:        scripts,
		createdAt:      createdAt,
		logFactory:     logFactory,
		logger:         logFactory.Logger(),
		preServices1:   preServices1,
		preServices2:   preServices2,
		postServices:   postServices,
		done:           make(chan struct{}),
		reloadChan:     reloadChan,
	}, nil
}

func (s *Box) PreStart() error {
	err := s.preStart()
	if err != nil {
		// TODO: remove catch error
		defer func() {
			v := recover()
			if v != nil {
				log.Error(E.Cause(err, "origin error"))
				debug.PrintStack()
				panic("panic on early close: " + fmt.Sprint(v))
			}
		}()
		s.Close()
		return err
	}
	s.logger.Info("sing-box pre-started (", F.Seconds(time.Since(s.createdAt).Seconds()), "s)")
	return nil
}

func (s *Box) Start() error {
	err := s.start()
	if err != nil {
		// TODO: remove catch error
		defer func() {
			v := recover()
			if v != nil {
				log.Error(E.Cause(err, "origin error"))
				debug.PrintStack()
				panic("panic on early close: " + fmt.Sprint(v))
			}
		}()
		s.Close()
		return err
	}
	s.logger.Info("sing-box started (", F.Seconds(time.Since(s.createdAt).Seconds()), "s)")
	return nil
}

func (s *Box) preStart() error {
	monitor := taskmonitor.New(s.logger, C.DefaultStartTimeout)
	monitor.Start("start logger")
	err := s.logFactory.Start()
	monitor.Finish()
	if err != nil {
		return E.Cause(err, "start logger")
	}
	for _, script := range s.scripts {
		err := script.PreStart()
		if err != nil {
			return E.Cause(err, "pre-start script[", script.Tag(), "]")
		}
	}
	for serviceName, service := range s.preServices1 {
		if preService, isPreService := service.(adapter.PreStarter); isPreService {
			monitor.Start("pre-start ", serviceName)
			err := preService.PreStart()
			monitor.Finish()
			if err != nil {
				return E.Cause(err, "pre-start ", serviceName)
			}
		}
	}
	for serviceName, service := range s.preServices2 {
		if preService, isPreService := service.(adapter.PreStarter); isPreService {
			monitor.Start("pre-start ", serviceName)
			err := preService.PreStart()
			monitor.Finish()
			if err != nil {
				return E.Cause(err, "pre-start ", serviceName)
			}
		}
	}
	err = s.startOutbounds()
	if err != nil {
		return err
	}
	return s.router.Start()
}

func (s *Box) start() error {
	err := s.preStart()
	if err != nil {
		return err
	}
	for serviceName, service := range s.preServices1 {
		err = service.Start()
		if err != nil {
			return E.Cause(err, "start ", serviceName)
		}
	}
	for serviceName, service := range s.preServices2 {
		err = service.Start()
		if err != nil {
			return E.Cause(err, "start ", serviceName)
		}
	}
	for serviceName, service := range s.preServices2 {
		s.logger.Trace("starting ", serviceName)
		err = service.Start()
		if err != nil {
			return E.Cause(err, "start ", serviceName)
		}
	}
	for _, proxyProvider := range s.proxyProviders {
		s.logger.Trace("starting proxyprovider ", proxyProvider.Tag())
		err = proxyProvider.Start()
		if err != nil {
			return E.Cause(err, "start proxyprovider ", proxyProvider.Tag())
		}
	}
	for i, in := range s.inbounds {
		var tag string
		if in.Tag() == "" {
			tag = F.ToString(i)
		} else {
			tag = in.Tag()
		}
		err = in.Start()
		if err != nil {
			return E.Cause(err, "initialize inbound/", in.Type(), "[", tag, "]")
		}
	}
	return s.postStart()
}

func (s *Box) postStart() error {
	for serviceName, service := range s.postServices {
		err := service.Start()
		if err != nil {
			return E.Cause(err, "start ", serviceName)
		}
	}
	for _, outbound := range s.outbounds {
		if lateOutbound, isLateOutbound := outbound.(adapter.PostStarter); isLateOutbound {
			err := lateOutbound.PostStart()
			if err != nil {
				return E.Cause(err, "post-start outbound/", outbound.Tag())
			}
		}
	}
	err := s.router.PostStart()
	if err != nil {
		return E.Cause(err, "post-start router")
	}
	for _, script := range s.scripts {
		err := script.PostStart()
		if err != nil {
			return E.Cause(err, "post-start script[", script.Tag(), "]")
		}
	}
	return nil
}

func (s *Box) Close() error {
	select {
	case <-s.done:
		return os.ErrClosed
	default:
		close(s.done)
	}
	monitor := taskmonitor.New(s.logger, C.DefaultStopTimeout)
	var errors error
	for _, script := range s.scripts {
		errors = E.Append(errors, script.PreClose(), func(err error) error {
			return E.Cause(err, "pre-close script[", script.Tag(), "]")
		})
	}
	for serviceName, service := range s.postServices {
		monitor.Start("close ", serviceName)
		errors = E.Append(errors, service.Close(), func(err error) error {
			return E.Cause(err, "close ", serviceName)
		})
		monitor.Finish()
	}
	for _, proxyProvider := range s.proxyProviders {
		s.logger.Trace("closing proxyprovider ", proxyProvider.Tag())
		errors = E.Append(errors, proxyProvider.Close(), func(err error) error {
			return E.Cause(err, "close proxyprovider ", proxyProvider.Tag())
		})
	}
	for i, in := range s.inbounds {
		monitor.Start("close inbound/", in.Type(), "[", i, "]")
		errors = E.Append(errors, in.Close(), func(err error) error {
			return E.Cause(err, "close inbound/", in.Type(), "[", i, "]")
		})
		monitor.Finish()
	}
	for i, out := range s.outbounds {
		monitor.Start("close outbound/", out.Type(), "[", i, "]")
		errors = E.Append(errors, common.Close(out), func(err error) error {
			return E.Cause(err, "close outbound/", out.Type(), "[", i, "]")
		})
		monitor.Finish()
	}
	monitor.Start("close router")
	if err := common.Close(s.router); err != nil {
		errors = E.Append(errors, err, func(err error) error {
			return E.Cause(err, "close router")
		})
	}
	monitor.Finish()
	for serviceName, service := range s.preServices1 {
		monitor.Start("close ", serviceName)
		errors = E.Append(errors, service.Close(), func(err error) error {
			return E.Cause(err, "close ", serviceName)
		})
		monitor.Finish()
	}
	for serviceName, service := range s.preServices2 {
		monitor.Start("close ", serviceName)
		errors = E.Append(errors, service.Close(), func(err error) error {
			return E.Cause(err, "close ", serviceName)
		})
		monitor.Finish()
	}
	for _, script := range s.scripts {
		errors = E.Append(errors, script.PostClose(), func(err error) error {
			return E.Cause(err, "post-close script[", script.Tag(), "]")
		})
	}
	s.logger.Trace("closing log factory")
	if err := common.Close(s.logFactory); err != nil {
		errors = E.Append(errors, err, func(err error) error {
			return E.Cause(err, "close logger")
		})
	}
	return errors
}

func (s *Box) Router() adapter.Router {
	return s.router
}

func (s *Box) ReloadChan() <-chan struct{} {
	return s.reloadChan
}
