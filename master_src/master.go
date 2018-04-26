package master

import (
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/GoodDeeds/load-balancer/common/constants"
	"github.com/GoodDeeds/load-balancer/common/logger"
	"github.com/GoodDeeds/load-balancer/common/packets"
	"github.com/GoodDeeds/load-balancer/common/utility"
	"github.com/op/go-logging"
)

// Master is used to store info of master node which is currently running
type Master struct {
	myIP            net.IP
	broadcastIP     net.IP
	slavePool       *SlavePool
	Logger          *logging.Logger
	tasks           map[int]MasterTask
	lastTaskId      int
	unackedSlaves   map[string]struct{}
	unackedSlaveMtx sync.RWMutex
	loadBalancer    LoadBalancerInterface

	close     chan struct{}
	closeWait sync.WaitGroup
}

// task as seen by master
type MasterTask struct {
	TaskId     int
	Task       string
	Load       int
	AssignedTo *Slave
	IsAssigned bool
	TaskStatus packets.Status
}

// master constructor
func (m *Master) initDS() {
	m.close = make(chan struct{})
	m.unackedSlaves = make(map[string]struct{})
	m.slavePool = &SlavePool{
		Logger: m.Logger,
	}
	m.tasks = make(map[int]MasterTask)
	m.lastTaskId = 0
	m.loadBalancer = &RoundRobin{&LoadBalancerBase{slavePool: m.slavePool}, -1}
}

// run master
func (m *Master) Run() {
	m.initDS()
	m.updateAddress()
	go m.connect()
	go m.gc_routine()
	m.Logger.Info(logger.FormatLogMessage("msg", "Master running"))
	time.Sleep(10 * time.Second)
	m.Logger.Info(logger.FormatLogMessage("msg", "Starting Tasks"))
	for i := 0; i < 10; i++ {
		m.assignNewTask("ABC", 10)
		time.Sleep(2 * time.Second)
	}
	m.Logger.Info(logger.FormatLogMessage("msg", "Tasks complete"))
	<-m.close
	m.Close()
}

func (m *Master) updateAddress() {
	ipnet, err := utility.GetMyIP()
	if err != nil {
		m.Logger.Fatal(logger.FormatLogMessage("msg", "Failed to get IP", "err", err.Error()))
	}

	m.myIP = ipnet.IP
	for i, b := range ipnet.Mask {
		m.broadcastIP = append(m.broadcastIP, (m.myIP[i] | (^b)))
	}
}

func (m *Master) SlaveExists(ip net.IP, id uint16) bool {
	return m.slavePool.SlaveExists(ip, id)
}

func (m *Master) gc_routine() {
	m.Logger.Info(logger.FormatLogMessage("msg", "Garbage collection routine started"))
	end := false
	for !end {
		select {
		case <-m.close:
			end = true
		default:
			m.slavePool.gc(m.Logger)
		}
		<-time.After(constants.GarbageCollectionInterval)
	}
}

func (m *Master) Close() {
	m.Logger.Info(logger.FormatLogMessage("msg", "Closing Master gracefully..."))
	select {
	case <-m.close:
	default:
		close(m.close)
	}
	m.slavePool.Close(m.Logger)
	m.closeWait.Wait()
}

// create task, find whom to assign, and send to that slave's channel
func (m *Master) assignNewTask(task string, load int) error {
	t := m.createTask(task, load)
	s := m.assignTask(t)
	m.Logger.Info(logger.FormatLogMessage("msg", "Assigned Task", "Task", task, "Slave", strconv.Itoa(int(s.id)))) // TODO - cast may not be correct
	p := m.assignTaskPacket(t)
	pt := packets.CreatePacketTransmit(p, packets.TaskRequest) // TODO - fix this
	s.sendChan <- pt
	//	var packetType packets.TaskRequestPacket
	//	s.sendChan <- packetType // TODO - this could cause issues, packaging with pt (above) would be better
	return nil
}

// Slave is used to store info of slave node connected to it
type Slave struct {
	ip          string
	id          uint16
	loadReqPort uint16
	reqSendPort uint16
	Logger      *logging.Logger
	maxLoad     int
	currentLoad int

	sendChan chan packets.PacketTransmit
	//	recvChan chan struct{}

	load              float64
	lastLoadTimestamp time.Time
	mtx               sync.RWMutex

	close     chan struct{}
	closeWait sync.WaitGroup
}

func (s *Slave) UpdateLoad(l float64, ts time.Time) {
	s.mtx.Lock()
	defer s.mtx.Unlock()
	if ts.After(s.lastLoadTimestamp) {
		s.load = l
		s.lastLoadTimestamp = ts
	}
}

func (s *Slave) InitDS() {
	s.close = make(chan struct{})
	s.sendChan = make(chan packets.PacketTransmit)
	s.currentLoad = 0
	s.maxLoad = 1000
	//	s.recvChan = make(chan struct{})
	//	go s.sendChannelHandler()
	//	go s.recvChannelHandler()
}

func (s *Slave) InitConnections() error {
	go s.loadRequestHandler()
	go s.taskRequestHandler()
	// go s.requestHandler()
	return nil
}

func (s *Slave) loadRequestHandler() {
	s.closeWait.Add(1)

	address := s.ip + ":" + strconv.Itoa(int(s.loadReqPort))
	// s.Logger.Info(logger.FormatLogMessage("loadReqPort", strconv.Itoa(int(s.loadReqPort)), "reqSendPort", strconv.Itoa(int(s.reqSendPort))))

	conn, err := net.Dial("tcp", address)
	if err != nil {
		close(s.close)
	}

	go s.loadRecvAndUpdater(conn)

	end := false
	for !end {
		select {
		case <-s.close:
			end = true
		default:
			packet := packets.LoadRequestPacket{}
			bytes, err := packets.EncodePacket(packet, packets.LoadRequest)
			if err != nil {
				s.Logger.Warning(logger.FormatLogMessage("msg", "Failed to encode packet",
					"slave_ip", s.ip, "err", err.Error()))
			}

			_, err = conn.Write(bytes)
			if err != nil {
				s.Logger.Warning(logger.FormatLogMessage("msg", "Failed to send LoadReq packet",
					"slave_ip", s.ip, "err", err.Error()))
			}

			<-time.After(constants.LoadRequestInterval)
		}
	}

	s.closeWait.Done()
}

func (s *Slave) loadRecvAndUpdater(conn net.Conn) {
	s.closeWait.Add(1)
	end := false
	for !end {
		select {
		case <-s.close:
			end = true
		default:
			var buf [2048]byte
			// TODO: add timeout
			n, err := conn.Read(buf[0:])
			if err != nil {
				s.Logger.Error(logger.FormatLogMessage("msg", "Error in reading from TCP"))
				if err == io.EOF {
					// TODO: remove myself from slavepool
					s.Logger.Warning(logger.FormatLogMessage("msg", "Closing a slave", "slave_ip", s.ip,
						"slave_id", strconv.Itoa(int(s.id))))
					close(s.close)
					end = true
				}
				continue
			}

			packetType, err := packets.GetPacketType(buf[:n])
			if err != nil {
				s.Logger.Error(logger.FormatLogMessage("err", err.Error()))
				return
			}

			switch packetType {
			case packets.LoadResponse:
				var p packets.LoadResponsePacket
				err := packets.DecodePacket(buf[:n], &p)
				if err != nil {
					s.Logger.Error(logger.FormatLogMessage("msg", "Failed to decode packet",
						"packet", packetType.String(), "err", err.Error()))
					return
				}

				s.UpdateLoad(p.Load, p.Timestamp)
				s.Logger.Info(logger.FormatLogMessage("msg", "Load updated",
					"slave_ip", s.ip, "slave_id", strconv.Itoa(int(s.id)), "load", strconv.FormatFloat(p.Load, 'E', -1, 64)))

			default:
				s.Logger.Warning(logger.FormatLogMessage("msg", "Received invalid packet"))
			}
		}
	}
	s.closeWait.Done()
}

func (s *Slave) taskRecvAndUpdater(conn net.Conn) {
	s.closeWait.Add(1)
	end := false
	for !end {
		select {
		case <-s.close:
			end = true
		default:
			var buf [2048]byte
			// TODO: add timeout
			n, err := conn.Read(buf[0:])
			if err != nil {
				s.Logger.Error(logger.FormatLogMessage("msg", "Error in reading from TCP"))
				if err == io.EOF {
					// TODO: remove myself from slavepool
					s.Logger.Warning(logger.FormatLogMessage("msg", "Closing a slave", "slave_ip", s.ip,
						"slave_id", strconv.Itoa(int(s.id))))
					close(s.close)
					end = true
				}
				continue
			}

			packetType, err := packets.GetPacketType(buf[:n])
			if err != nil {
				s.Logger.Error(logger.FormatLogMessage("err", err.Error()))
				return
			}

			switch packetType {
			case packets.TaskRequestResponse:
				var p packets.TaskRequestResponsePacket
				err := packets.DecodePacket(buf[:n], &p)
				if err != nil {
					s.Logger.Error(logger.FormatLogMessage("msg", "Failed to decode packet",
						"packet", packetType.String(), "err", err.Error()))
					return
				}

				go s.handleTaskRequestResponse(p)

			case packets.TaskResultResponse:
				var p packets.TaskResultResponsePacket
				err := packets.DecodePacket(buf[:n], &p)
				if err != nil {
					s.Logger.Error(logger.FormatLogMessage("msg", "Failed to decode packet",
						"packet", packetType.String(), "err", err.Error()))
					return
				}

				go s.handleTaskResult(p)

			case packets.TaskStatusResponse:
				var p packets.TaskStatusResponsePacket
				err := packets.DecodePacket(buf[:n], &p)
				if err != nil {
					s.Logger.Error(logger.FormatLogMessage("msg", "Failed to decode packet",
						"packet", packetType.String(), "err", err.Error()))
					return
				}

				go s.handleTaskStatusResponse(p)

			default:
				s.Logger.Warning(logger.FormatLogMessage("msg", "Received invalid packet"))
			}
		}
	}
	s.closeWait.Done()
}

func (s *Slave) taskRequestHandler() {
	s.closeWait.Add(1)

	address := s.ip + ":" + strconv.Itoa(int(s.reqSendPort))
	// s.Logger.Info(logger.FormatLogMessage("loadReqPort", strconv.Itoa(int(s.loadReqPort)), "reqSendPort", strconv.Itoa(int(s.reqSendPort))))

	conn, err := net.Dial("tcp", address)
	if err != nil {
		s.Logger.Fatal(logger.FormatLogMessage("Error!", "Error!"))
		close(s.close)
	}

	go s.sendChannelHandler(conn)
	go s.taskRecvAndUpdater(conn)

	s.closeWait.Done()
}

// func (s *Slave) requestHandler() {
// 	s.closeWait.Add(1)
// 	s.closeWait.Done()
// }

func (s *Slave) Close() {
	close(s.close)
	s.closeWait.Wait()
}

func (s *Slave) sendChannelHandler(conn net.Conn) {
	s.closeWait.Add(1)
	end := false
	for !end {
		select {
		case <-s.close:
			end = true
		default:
			pt := <-s.sendChan
			bytes, err := packets.EncodePacket(pt.Packet, pt.PacketType)
			if err != nil {
				s.Logger.Error(logger.FormatLogMessage("msg", "Error in reading packet to send"))
				if err == io.EOF {
					// TODO: remove myself from slavepool
					s.Logger.Warning(logger.FormatLogMessage("msg", "Closing a slave", "slave_ip", s.ip,
						"slave_id", strconv.Itoa(int(s.id))))
					close(s.close)
					end = true
				}
				continue
			}
			_, err = conn.Write(bytes)
			if err != nil {
				s.Logger.Warning(logger.FormatLogMessage("msg", "Failed to send packet",
					"slave_ip", s.ip, "err", err.Error()))
			}

			<-time.After(constants.TaskInterval)

		}
	}
	s.closeWait.Done()
}

// TODO: regularly send info request to all slaves.

type SlavePool struct {
	mtx    sync.RWMutex
	slaves []*Slave
	Logger *logging.Logger
}

func (sp *SlavePool) AddSlave(slave *Slave) {
	// TODO: make connection with slave over the listeners.
	slave.Logger = sp.Logger
	slave.InitConnections()
	slave.InitDS()
	sp.mtx.Lock()
	defer sp.mtx.Unlock()
	sp.slaves = append(sp.slaves, slave)
}

func (sp *SlavePool) RemoveSlave(ip string) bool {
	sp.mtx.Lock()
	defer sp.mtx.Unlock()
	toRemove := -1
	for i, slave := range sp.slaves {
		if slave.ip == ip {
			toRemove = i
			break
		}
	}

	if toRemove < 0 {
		return false
	}

	sp.slaves = append(sp.slaves[:toRemove], sp.slaves[toRemove+1:]...)
	return true
}

func (sp *SlavePool) SlaveExists(ip net.IP, id uint16) bool {
	sp.mtx.RLock()
	defer sp.mtx.RUnlock()
	ipStr := ip.String()
	for _, slave := range sp.slaves {
		if slave.ip == ipStr && slave.id == id {
			return true
		}
	}
	return false
}

func (sp *SlavePool) Close(log *logging.Logger) {
	// close all go routines/listeners
	log.Info(logger.FormatLogMessage("msg", "Closing Slave Pool"))
	for _, s := range sp.slaves {
		s.Close()
	}
}

func (sp *SlavePool) gc(log *logging.Logger) {
	toRemove := []int{}
	for i, slave := range sp.slaves {
		select {
		case <-slave.close:
			slave.closeWait.Wait()
			toRemove = append(toRemove, i)
		default:
		}
	}

	if len(toRemove) > 0 {
		sp.mtx.Lock()
		defer sp.mtx.Unlock()

		for i, idx := range toRemove {
			log.Info(logger.FormatLogMessage("msg", "Slave removed in gc",
				"slave_ip", sp.slaves[idx-i].ip, "slave_id", strconv.Itoa(int(sp.slaves[idx-i].id))))
			sp.slaves = append(sp.slaves[:idx-i], sp.slaves[idx+1-i:]...)
		}

	}
}
