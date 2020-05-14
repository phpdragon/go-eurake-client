package eureka

import (
	"fmt"
	core "github.com/phpdragon/go-eurake-client/core"
	log "github.com/phpdragon/go-eurake-client/log"
	netUtil "github.com/phpdragon/go-eurake-client/netutil"
	"go.uber.org/atomic"
	"go.uber.org/zap"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	defaultSleepIntervals = 3
	//
	httpPrefix  = "http://"
	httpsPrefix = "https://"
	//
	httpKey  = 0
	httpsKey = 1
)

type Client struct {
	Running bool

	//自增器
	autoInc *atomic.Int64

	// for monitor system signal
	signalChan chan os.Signal

	//日志对象
	logger *log.ClientLogger

	mutex sync.RWMutex

	config *Config

	// current client (instance) config
	instance *core.Instance

	// applications registry
	// key: appId
	// value: Application
	registryAppMap map[string]*core.Application

	// instances registry
	// key: appId
	// value:
	//		key:  int(0...n)
	//		value: InstanceConfig
	activeInstanceMap map[string]map[int]*core.Instance

	// instance real url map
	// key: appId
	// value:
	//		key:  int(http:0, https:1)
	//		value:
	//			key:  int(0...n)
	//			value: real url
	activeServiceIpPortMap map[string]map[int]map[int]string
}

func NewClient(config *Config) *Client {
	return NewClientWithLog(config, nil)
}

func NewClientWithLog(config *Config, zapLog *zap.Logger) *Client {
	instanceConfig, _ := NewInstance(config)

	client := &Client{
		//自增器
		autoInc:    atomic.NewInt64(0),
		logger:     log.NewLogAgent(zapLog),
		signalChan: make(chan os.Signal),
		//
		config:   config,
		instance: instanceConfig,
	}

	return client
}

func (client *Client) Run() {
	client.mutex.Lock()
	client.Running = true
	client.mutex.Unlock()

	// handle exit signal to de-register instance
	go client.handleSignal()

	// (if FetchRegistry is true), fetch registry apps periodically
	// and update to t.registryAppMap
	go client.refreshRegistry()

	client.registerWithEureka()
}

func (client *Client) Shutdown() {
	//client在shutdown情况下，是否显示从注册中心注销
	if !client.Running || !client.config.ClientConfig.ShouldUnregisterOnShutdown {
		return
	}

	client.logger.Info(fmt.Sprintf("Receive exit signal, client instance going to de-register, instanceId=%s.", client.instance.InstanceId))
	// de-register instance
	api, err := client.Api()
	if err != nil {
		client.logger.Error(fmt.Sprintf("Failed to get EurekaServerApi instance, de-register %s failed, err=%s", client.instance.InstanceId, err.Error()))
		return
	}
	err = api.DeRegisterInstance(client.instance.App, client.instance.InstanceId)
	if err != nil {
		client.logger.Error(fmt.Sprintf("Failed to de-register %s, err=%s", client.instance.InstanceId, err.Error()))
		return
	}

	client.mutex.Lock()
	client.Running = false
	client.mutex.Unlock()

	client.logger.Info(fmt.Sprintf("de-register %s success.", client.instance.InstanceId))
}

func (client *Client) GetApplications() map[string]*core.Application {
	return client.registryAppMap
}

func (client *Client) GetInstances() map[string]map[int]*core.Instance {
	return client.activeInstanceMap
}

//获取下一个容器
func (client *Client) GetNextServerFromEureka(appId string) (*core.Instance, error) {
	instanceMap, err := client.getActiveInstancesByAppId(appId)
	if nil != err {
		return &core.Instance{}, err
	}

	if nil == instanceMap || 0 == len(instanceMap) {
		client.logger.Error(fmt.Sprintf("This %s instances not exist!", appId))
		return &core.Instance{}, fmt.Errorf("This %s instances not exist!", appId)
	}

	index := client.getRandIndex(len(instanceMap))
	return instanceMap[index], nil
}

func (client *Client) getRandIndex(total int) int {
	var index64 = client.autoInc.Inc() % int64(total)
	return *(*int)(unsafe.Pointer(&index64))
}

func (client *Client) GetRealHttpUrl(httpUrl string) (string, error) {
	httpUrlTmp := strings.Replace(httpUrl, httpPrefix, "", -1)
	httpUrlTmp = strings.Replace(httpUrlTmp, httpsPrefix, "", -1)
	urls := strings.Split(httpUrlTmp, "/")
	appName := urls[0]

	//是否https
	mapKey := httpKey
	if strings.HasPrefix(httpUrl, httpsPrefix) {
		mapKey = httpsKey
	}

	ipPortMap, err := client.getActiveServiceIpPortByAppId(appName)
	if nil != err || 0 == len(ipPortMap) {
		//TODO：文案
		return "", fmt.Errorf("This %s instances not exist!", appName)
	}

	//取http还是https的ip:port
	realIpPorts := ipPortMap[mapKey]
	if nil == realIpPorts || 0 == len(realIpPorts) {
		//TODO：文案
		return "", fmt.Errorf("This %s instances not exist!", appName)
	}

	//随机取一个目标ip:port
	index := client.getRandIndex(len(realIpPorts))
	realIpPort := realIpPorts[index]

	return strings.Replace(httpUrl, appName, realIpPort, -1), nil
}

func (client *Client) getActiveInstancesByAppId(appId string) (map[int]*core.Instance, error) {
	id := strings.ToUpper(appId)
	cache := client.activeInstanceMap[id]
	if nil != cache {
		return client.activeInstanceMap[id], nil
	}

	err := client.doRefreshByAppId(appId)
	if nil != err {
		return nil, err
	}

	return client.activeInstanceMap[id], nil
}

func (client *Client) getActiveServiceIpPortByAppId(appId string) (map[int]map[int]string, error) {
	id := strings.ToUpper(appId)
	cache := client.activeServiceIpPortMap[id]
	if nil != cache {
		return client.activeServiceIpPortMap[id], nil
	}

	err := client.doRefreshByAppId(appId)
	if nil != err {
		return nil, err
	}

	return client.activeServiceIpPortMap[id], nil
}

func (client *Client) doRefreshByAppId(appId string) error {
	api, err := client.Api()
	if err != nil {
		return err
	}

	application, errr := api.QueryAllInstanceByAppId(appId)
	if errr != nil {
		return errr
	}

	instances, urls := getActiveInstancesAndIpPorts(client.config.ClientConfig.FilterOnlyUpInstances, application.Instances)

	client.mutex.Lock()
	defer client.mutex.Unlock()

	client.registryAppMap[appId] = application
	client.activeInstanceMap[appId] = instances
	client.activeServiceIpPortMap[appId] = urls

	return nil
}

func (client *Client) refreshRegistry() {
	if !client.config.ClientConfig.FetchRegistry {
		return
	}

	for {
		_ = client.fetchRegistry()
		time.Sleep(time.Second * time.Duration(client.config.ClientConfig.getRegistryFetchIntervalSeconds()))
	}
}

//刷新服务列表
func (client *Client) fetchRegistry() error {
	client.logger.Info("Fetch registry info")

	api, err := client.Api()
	if err != nil {
		client.logger.Error(fmt.Sprintf("Failed to QueryAllInstances, err=%s", err.Error()))
		return err
	}

	apps, err := api.QueryAllInstances()
	if err != nil {
		client.logger.Error(fmt.Sprintf("Failed to QueryAllInstances, err=%s", err.Error()))
		return err
	}

	registryApps := make(map[string]*core.Application)
	activeInstances := make(map[string]map[int]*core.Instance)
	activeServiceUrls := make(map[string]map[int]map[int]string)

	for _, app := range apps.Applications {
		instances, urls := getActiveInstancesAndIpPorts(client.config.ClientConfig.FilterOnlyUpInstances, app.Instances)
		registryApps[app.Name] = &app
		activeInstances[app.Name] = instances
		activeServiceUrls[app.Name] = urls
	}

	client.mutex.Lock()
	defer client.mutex.Unlock()

	client.registryAppMap = registryApps
	client.activeInstanceMap = activeInstances
	client.activeServiceIpPortMap = activeServiceUrls

	return nil
}

// register instance (default current status is STARTING)
// and update instance status to UP
func (client *Client) registerWithEureka() {
	if !client.config.ClientConfig.RegisterWithEureka {
		client.logger.Warn("This instance don't register to eureka!")
		return
	}

	for {
		if client.instance == nil {
			client.logger.Error("Config instance can't be nil")
			return
		}

		api, err := client.Api()
		if err != nil {
			time.Sleep(time.Second * defaultSleepIntervals)
			continue
		}

		err = api.RegisterInstance(client.instance.App, client.instance)
		if err != nil {
			client.logger.Error(fmt.Sprintf("ClientConfig register failed, err=%s", err.Error()))
			time.Sleep(time.Second * defaultSleepIntervals)
			continue
		}
		client.logger.Info(fmt.Sprintf("Successfully register service to eureka with status[%s] !", client.instance.Status))

		break
	}

	go func() {
		for {
			enabledOnInit := client.config.InstanceConfig.InstanceEnabledOnInit
			if enabledOnInit || (!enabledOnInit && client.serverIsStarted()) {
				updated, err := client.updateInstanceStatus()
				if nil != err {
					client.logger.Error(err.Error())
				}
				if updated {
					break
				}
			}
			time.Sleep(time.Second * defaultSleepIntervals)
		}
	}()

	//发送心跳
	go client.heartbeat()
}

//判断http服务是否已经启动
func (client *Client) serverIsStarted() bool {
	port := client.instance.Port.Port
	if "true" == client.instance.SecurePort.Enabled {
		port = client.instance.SecurePort.Port
	}

	used := netUtil.PortInUse(client.instance.IpAddr, port)
	client.logger.Debug(fmt.Sprintf("Check that the web server is started, result:%t", used))

	return used
}

func (client *Client) PortInUse(host string, ports []string) bool {
	for _, port := range ports {
		timeout := time.Second
		conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), timeout)
		if err != nil {
			return false
		}
		if conn != nil {
			defer conn.Close()
			return true
		}
	}

	return false
}

func (client *Client) updateInstanceStatus() (bool, error) {
	client.logger.Info("Update the instance status to UP ...")

	if client.instance == nil {
		client.logger.Error("Config instance can't be nil")
		return false, nil
	}

	api, err := client.Api()
	if err != nil {
		return false, nil
	}

	//如果成功注册到eureka并将状态更新到UP
	// if success to register to eureka and update status to UP
	// then break loop
	err = api.UpdateInstanceStatus(client.instance.App, client.instance.InstanceId, core.STATUS_UP)
	if err != nil {
		client.logger.Error(fmt.Sprintf("ClientConfig UP failed, err=%s", err.Error()))
		return false, nil
	}

	client.logger.Info("The server status[UP] was updated successfully !")

	return true, nil
}

// Api for sending rest httpClient to eureka server
func (client *Client) Api() (*core.EurekaServerApi, error) {
	api, err := client.pickEurekaServerApi()
	if err != nil {
		return nil, err
	}
	return api, nil
}

//TODO:
// rand to pick service url and new EurekaServerApi instance
func (client *Client) pickEurekaServerApi() (*core.EurekaServerApi, error) {
	return core.NewEurekaServerApi(client.config.ServiceURL.DefaultZone), nil
}

// 发送心跳
// eureka client heartbeat
func (client *Client) heartbeat() {
	for {
		api, err := client.Api()
		if err != nil {
			time.Sleep(time.Second * defaultSleepIntervals)
			continue
		}

		err = api.SendHeartbeat(client.instance.App, client.instance.InstanceId)
		if err != nil {
			client.logger.Error(fmt.Sprintf("Failed to send heartbeat, err=%s", err.Error()))
			time.Sleep(time.Second * defaultSleepIntervals)
			continue
		}

		client.logger.Debug(fmt.Sprintf("Heartbeat app=%s, instanceId=%s", client.instance.App, client.instance.InstanceId))
		time.Sleep(time.Duration(client.config.InstanceConfig.LeaseInfo.RenewalIntervalInSecs) * time.Second)
	}
}

//获取有效的实例和链接
func getActiveInstancesAndIpPorts(filterOnlyUpInstances bool, instances []core.Instance) (map[int]*core.Instance, map[int]map[int]string) {
	instancesX := make(map[int]*core.Instance)
	//
	urls := make(map[int]map[int]string)
	httpUrls := make(map[int]string)
	httpsUrls := make(map[int]string)
	instanceTotal := len(instances)
	for i := 0; i < instanceTotal; i++ {
		instance := instances[i]

		if filterOnlyUpInstances && core.STATUS_UP != instance.Status {
			continue
		}

		instancesX[i] = &instance

		if "true" == instance.Port.Enabled {
			httpUrls[i] = fmt.Sprintf("%s:%d", instance.IpAddr, instance.Port.Port)
		}
		if "false" == instance.SecurePort.Enabled {
			httpsUrls[i] = fmt.Sprintf("%s:%d", instance.IpAddr, instance.SecurePort.Port)
		}
	}

	urls[httpKey] = httpUrls
	urls[httpsKey] = httpsUrls
	return instancesX, urls
}

// for graceful kill. Here handle SIGTERM signal to do sth
// e.g: kill -TERM $pid
//      or "ctrl + c" to exit
func (client *Client) handleSignal() {
	if client.signalChan == nil {
		client.signalChan = make(chan os.Signal)
	}

	signal.Notify(client.signalChan, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)

	for {
		switch <-client.signalChan {
		case syscall.SIGINT:
			client.logger.Info(fmt.Sprintf("syscall.SIGINT, instanceId=%s.", client.instance.InstanceId))
			fallthrough
		case syscall.SIGKILL:
			client.logger.Info(fmt.Sprintf("syscall.SIGKILL, instanceId=%s.", client.instance.InstanceId))
			fallthrough
		case syscall.SIGHUP:
			client.logger.Info(fmt.Sprintf("syscall.SIGHUP, instanceId=%s.", client.instance.InstanceId))
			fallthrough
		case syscall.SIGQUIT:
			client.logger.Info(fmt.Sprintf("syscall.SIGQUIT, instanceId=%s.", client.instance.InstanceId))
			fallthrough
		case syscall.SIGTERM:
			client.Shutdown()
			os.Exit(0)
		}
	}
}