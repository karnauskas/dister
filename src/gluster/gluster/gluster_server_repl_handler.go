// 数据同步需要注意的是：
// leader只有在通知完所有follower更新完数据之后，自身才会进行数据更新
// 因此leader
package gluster

import (
    "net"
    "g/encoding/gjson"
    "g/core/types/gmap"
    "g/util/gtime"
    "g/os/glog"
    "sync/atomic"
    "time"
    "fmt"
    "strconv"
)

// 集群数据同步接口回调函数
func (n *Node) replTcpHandler(conn net.Conn) {
    msg := n.receiveMsg(conn)
    if msg == nil || msg.Info.Group != n.Group {
        //glog.Println("receive nil, auto close conn")
        conn.Close()
        return
    }
    switch msg.Head {
        case gMSG_REPL_DATA_SET:                    n.onMsgReplDataSet(conn, msg)
        case gMSG_REPL_DATA_REMOVE:                 n.onMsgReplDataRemove(conn, msg)
        case gMSG_API_PEERS_ADD:                    n.onMsgApiPeersAdd(conn, msg)
        case gMSG_API_PEERS_REMOVE:                 n.onMsgApiPeersRemove(conn, msg)
        case gMSG_REPL_PEERS_UPDATE:                n.onMsgPeersUpdate(conn, msg)
        case gMSG_REPL_DATA_UPDATE_CHECK:           n.onMsgDataUpdateCheck(conn, msg)
        case gMSG_REPL_DATA_UNCOMMITED_LOG_ENTRY:   n.onMsgUncommittedLogEntry(conn, msg)
        case gMSG_REPL_DATA_APPEND_LOG_ENTRY:       n.onMsgAppendLogEntry(conn, msg)
        case gMSG_REPL_DATA_INCREMENTAL_UPDATE:     n.onMsgReplUpdate(conn, msg)
        case gMSG_REPL_CONFIG_FROM_FOLLOWER     :   n.onMsgConfigFromFollower(conn, msg)
        case gMSG_REPL_SERVICE_COMPLETELY_UPDATE:   n.onMsgServiceCompletelyUpdate(conn, msg)
        case gMSG_API_SERVICE_SET:                  n.onMsgServiceSet(conn, msg)
        case gMSG_API_SERVICE_REMOVE:               n.onMsgServiceRemove(conn, msg)
    }
    //这里不用自动关闭链接，由于链接有读取超时，当一段时间没有数据时会自动关闭
    n.replTcpHandler(conn)
}

// kv设置，最终一致性
// 这里增加了一把数据锁，以保证请求的先进先出队列执行，因此写效率会有所降低
func (n *Node) onMsgReplDataSet(conn net.Conn, msg *Msg) {
    result := gMSG_REPL_FAILED
    if n.getRaftRole() == gROLE_RAFT_LEADER {
        var items interface{}
        if gjson.DecodeTo(msg.Body, &items) == nil {
            n.dmutex.Lock()
            var entry = LogEntry {
                Id    : n.makeLogId(),
                Act   : msg.Head,
                Items : items,
            }
            // 先保存日志，当节点同步成功后才真实写入数据
            // 不用做重复性验证，即使失败了，客户端也需要做重试请求
            // 失败情况下日志只会不断追加，但并不会影响最终的数据结果
            if n.sendUncommittedLogEntryToPeers(entry, msg.Info.Id) {
                go n.sendAppendLogEntryToPeers(entry.Id)
                n.LogList.PushFront(&entry)
                n.saveLogEntry(&entry)
                result = gMSG_REPL_RESPONSE
            }
            n.dmutex.Unlock()
        }
    }
    n.sendMsg(conn, result, "")
}

// 确认写入日志
func (n *Node) onMsgAppendLogEntry(conn net.Conn, msg *Msg) {
    v := n.UncommittedLogs.Get(msg.Body)
    if v != nil {
        entry := v.(LogEntry)
        n.dmutex.Lock()
        n.LogList.PushFront(&entry)
        n.saveLogEntry(&entry)
        n.dmutex.Unlock()
        n.UncommittedLogs.Remove(msg.Body)
    }
    n.sendMsg(conn, gMSG_REPL_RESPONSE, "")
}

// 新增数据日志
func (n *Node) onMsgUncommittedLogEntry(conn net.Conn, msg *Msg) {
    var entry LogEntry
    result := gMSG_REPL_FAILED
    if n.getLastLogId() == msg.Info.LastLogId && gjson.DecodeTo(msg.Body, &entry) == nil {
        n.UncommittedLogs.Set(strconv.FormatInt(entry.Id, 10), entry, 10)
        result = gMSG_REPL_RESPONSE
    }
    n.sendMsg(conn, result, "")
}

// Follower->Leader的配置同步
func (n *Node) onMsgConfigFromFollower(conn net.Conn, msg *Msg) {
    //glog.Println("config replication from", msg.Info.Name)
    j := gjson.DecodeToJson(msg.Body)
    if j != nil {
        // 初始化节点列表，包含自定义的所需添加的服务器IP或者域名列表
        peers := j.GetArray("Peers")
        if peers != nil {
            for _, v := range peers {
                ip := v.(string)
                if ip == n.Ip || n.Peers.Contains(ip) {
                    continue
                }
                go func(ip string) {
                    if !n.sayHi(ip) {
                        n.updatePeerInfo(NodeInfo{Id: ip, Ip: ip})
                    }
                }(ip)
            }
        }
    }
    conn.Close()
    //glog.Println("config replication from", msg.Info.Name, "done")
}

// Leader同步Peers信息到Follower
func (n *Node) onMsgPeersUpdate(conn net.Conn, msg *Msg) {
    //glog.Println("receive peers update", msg.Body)
    m  := make([]NodeInfo, 0)
    id := n.getId()
    ip := n.getIp()
    if gjson.DecodeTo(msg.Body, &m) == nil {
        for _, v := range m {
            if v.Id != id {
                n.updatePeerInfo(v)
            } else if v.Ip != ip {
                n.setIp(v.Ip)
            }
        }
    }
    conn.Close()
}

// 心跳消息提交的完整更新消息
func (n *Node) onMsgServiceCompletelyUpdate(conn net.Conn, msg *Msg) {
    n.updateServiceFromRemoteNode(conn, msg)
}

// Service删除
func (n *Node) onMsgServiceRemove(conn net.Conn, msg *Msg) {
    list := make([]string, 0)
    if gjson.DecodeTo(msg.Body, &list) == nil {
        if n.removeServiceByNames(list) {
            n.setLastServiceLogId(gtime.Microsecond())
        }
    }
    n.sendMsg(conn, gMSG_REPL_RESPONSE, "")
}

// Service设置
func (n *Node) onMsgServiceSet(conn net.Conn, msg *Msg) {
    var sc ServiceConfig
    if gjson.DecodeTo(msg.Body, &sc) == nil {
        n.removeServiceByNames([]string{sc.Name})
        for k, v := range sc.Node {
            key := n.getServiceKeyByNameAndIndex(sc.Name, k)
            n.Service.Set(key, Service{ sc.Type, v })
        }
        n.setLastServiceLogId(gtime.Microsecond())
    }
    n.sendMsg(conn, gMSG_REPL_RESPONSE, "")
}

// kv删除
func (n *Node) onMsgReplDataRemove(conn net.Conn, msg *Msg) {
    n.onMsgReplDataSet(conn, msg)
}

// Data是否需要同步检测
func (n *Node) onMsgDataUpdateCheck(conn net.Conn, msg *Msg) {
    result := gMSG_REPL_FAILED
    if n.getLastLogId() < msg.Info.LastLogId && n.UncommittedLogs.Size() == 0 {
        result = gMSG_REPL_RESPONSE
    }
    n.sendMsg(conn, result, "")
}

// 发送数据操作到其他节点,为保证数据的一致性和可靠性，只要请求节点及另外1个server节点返回成功后，我们就认为该数据请求成功。
// 1、保证请求节点的成功是为了让客户端在请求成功之后的下一次(本地请求)请求能优先获取到最新的修改；
// 2、即使在处理过程中leader挂掉，只要有另外一个server节点有最新的请求数据，那么就算重新进行选举，也会选举到数据最新的那个server节点作为leader,这里的机制类似于主从备份的原理；
// 3、此外，由于采用了异步并发请求的机制，如果集群存在多个其他server节点，出现仅有一个节点成功的概念很小，出现所有节点都失败的概率更小；
func (n *Node) sendUncommittedLogEntryToPeers(entry LogEntry, clientId string) bool {
    // 只有一个leader节点，并且配置是允许单节点运行
    if n.Peers.Size() < 1 {
        if n.getRaftRole() == gROLE_RAFT_LEADER && n.getMinNode() == 1 {
            return true
        } else {
            return false
        }
    }

    var clientok   int32 = 0 // 是否完成请求节点请求成功
    var serverok   int32 = 0 // 是否完成1个server请求成功
    var failCount  int32 = 0 // 失败请求数
    var doneCount  int32 = 0 // 成功请求数
    var aliveCount int32 = 0 // 总共发送的请求 = 失败请求数 + 成功请求数
    // 如果本地就是leader
    if clientId == n.getId() {
        clientok = 1
    }
    result := false
    for _, v := range n.Peers.Values() {
        info := v.(NodeInfo)
        if info.Status != gSTATUS_ALIVE {
            continue
        }
        aliveCount++
        go func(info *NodeInfo, entry *LogEntry) {
            conn := n.getConn(info.Ip, gPORT_REPL)
            if conn == nil {
                atomic.AddInt32(&failCount, 1)
                return
            }
            defer conn.Close()
            if n.sendMsg(conn, gMSG_REPL_DATA_UNCOMMITED_LOG_ENTRY, gjson.Encode(*entry)) == nil {
                msg := n.receiveMsg(conn)
                if msg != nil && msg.Head == gMSG_REPL_RESPONSE {
                    if msg.Info.Id == clientId {
                        atomic.AddInt32(&clientok, 1)
                    }
                    if msg.Info.Role == gROLE_SERVER {
                        atomic.AddInt32(&serverok, 1)
                    }
                    atomic.AddInt32(&doneCount, 1)
                } else {
                    atomic.AddInt32(&failCount, 1)
                }
            }
        }(&info, &entry)
    }
    // 等待执行结束
    timeout := gtime.Second() + 3
    for aliveCount > 0 {
        if atomic.LoadInt32(&clientok) > 0 && atomic.LoadInt32(&serverok) > 0 {
            result = true
            break;
        }
        if atomic.LoadInt32(&doneCount) == aliveCount {
            result = true
            break;
        }
        if atomic.LoadInt32(&failCount) == aliveCount {
            break;
        }
        if atomic.LoadInt32(&failCount) > 0 && (atomic.LoadInt32(&failCount) + atomic.LoadInt32(&doneCount) == aliveCount) {
            break;
        }
        if gtime.Second() >= timeout {
            break;
        }
        time.Sleep(time.Millisecond)
    }

    return result
}

// 向集群节点确认数据提交AppendEntries
func (n *Node) sendAppendLogEntryToPeers(logid int64) {
    for _, v := range n.Peers.Values() {
        info := v.(NodeInfo)
        if info.Status != gSTATUS_ALIVE {
            continue
        }
        go func(ip string) {
            try := 0
            for {
                if n.sendAppendLogEntryToPeer(ip, logid) {
                    break;
                } else {
                    try++
                    if try == 3 {
                        break
                    }
                }
            }
        }(info.Ip)
    }
}

// 向节点发送AppendEntry请求
func (n *Node) sendAppendLogEntryToPeer(ip string, logid int64) bool {
    result  := false
    tryConn := 0
    for !result {
        conn := n.getConn(ip, gPORT_REPL)
        if conn != nil {
            tryMsg := 0
            for !result {
                if n.sendMsg(conn, gMSG_REPL_DATA_APPEND_LOG_ENTRY, strconv.FormatInt(logid, 10)) != nil {
                    msg := n.receiveMsg(conn)
                    if msg != nil && msg.Head == gMSG_REPL_RESPONSE {
                        result = true
                        break;
                    }
                }
                tryMsg++
                if tryMsg == 3 {
                    break;
                }
            }
            if tryMsg == 3 {
                break;
            }
        } else {
            tryConn++
            if tryConn == 3 {
                break;
            }
        }
    }
    return false
}

// 数据同步，更新本地数据
func (n *Node) onMsgReplUpdate(conn net.Conn, msg *Msg) {
    glog.Println("receive data replication update from:", msg.Info.Name)
    n.updateDataFromRemoteNode(conn, msg)
    n.sendMsg(conn, gMSG_REPL_RESPONSE, "")
}

// 保存日志数据
func (n *Node) saveLogEntry(entry *LogEntry) {
    lastLogId := n.getLastLogId()
    if entry.Id < lastLogId {
        glog.Warning(fmt.Sprintf("expired log entry, received:%v, current:%v\n", entry.Id, lastLogId))
        return
    }
    switch entry.Act {
        case gMSG_REPL_DATA_SET:
            for k, v := range entry.Items.(map[string]interface{}) {
                n.DataMap.Set(k, v.(string))
            }

        case gMSG_REPL_DATA_REMOVE:
            for _, v := range entry.Items.([]interface{}) {
                n.DataMap.Remove(v.(string))
            }

    }
    n.setLastLogId(entry.Id)
}

// 从目标节点同步数据，采用增量模式
// follower<-leader
func (n *Node) updateDataFromRemoteNode(conn net.Conn, msg *Msg) {
    if n.getLastLogId() < msg.Info.LastLogId {
        array := make([]LogEntry, 0)
        err   := gjson.DecodeTo(msg.Body, &array)
        if err != nil {
            glog.Error(err)
            return
        }
        if array != nil && len(array) > 0 {
            for _, v := range array {
                if v.Id > n.getLastLogId() {
                    n.LogList.PushFront(&v)
                    n.saveLogEntry(&v)
                }
            }
        }
    }
}

// 同步数据到目标节点，采用增量模式
// leader->follower
func (n *Node) updateDataToRemoteNode(conn net.Conn, info *NodeInfo) {
    // 支持分批同步，如果数据量大，每一次增量同步大小不超过1万条
    logid := info.LastLogId
    if n.getLastLogId() > info.LastLogId {
        if logid == 0 || (logid != 0 && n.isValidLogId(logid)) {
            glog.Println("start data incremental replication from", n.getName(), "to", info.Name)
            for {
                list    := n.getLogEntriesByLastLogId(logid, 10000)
                length  := len(list)
                if length > 0 {
                    glog.Println("data incremental replication start logid:", list[0].Id, ", end logid:", list[length-1].Id)
                    if err := n.sendMsg(conn, gMSG_REPL_DATA_INCREMENTAL_UPDATE, gjson.Encode(list)); err != nil {
                        glog.Error(err)
                        return
                    }
                    if rmsg := n.receiveMsg(conn); rmsg != nil {
                        logid = rmsg.Info.LastLogId
                        if n.getLastLogId() == logid {
                            break;
                        }
                    } else {
                        break
                    }
                } else {
                    glog.Println("data incremental replication failed, logid:", logid)
                    break
                }
            }
        } else {
            glog.Println("failed in data replication from", n.getName(), "to", info.Name, ", invalid log id:", logid)
        }
    }
}

// 从目标节点同步Service数据
// follower<-leader
func (n *Node) updateServiceFromRemoteNode(conn net.Conn, msg *Msg) {
    defer conn.Close()
    m   := make(map[string]Service)
    err := gjson.DecodeTo(msg.Body, &m)
    if err == nil {
        newm := gmap.NewStringInterfaceMap()
        for k, v := range m {
            newm.Set(k, v)
        }
        n.setService(newm)
        n.setLastServiceLogId(msg.Info.LastServiceLogId)
    } else {
        glog.Error(err)
    }
}

// 同步Service到目标节点
// leader->follower
func (n *Node) updateServiceToRemoteNode(conn net.Conn) {
    defer conn.Close()
    if err := n.sendMsg(conn, gMSG_REPL_SERVICE_COMPLETELY_UPDATE, gjson.Encode(n.Service)); err != nil {
        glog.Error(err)
        return
    }
}

