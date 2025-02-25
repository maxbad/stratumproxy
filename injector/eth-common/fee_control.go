package eth_common

import (
	"github.com/sirupsen/logrus"
	"sort"
	"stratumproxy/connection"
	"sync"
	"time"
)

// EthFeeController 整体逻辑
// 1. 一个大循环 循环所有抽水的
// 2. 每隔 [6] 分钟检测抽水比例
// 3. 只要抽到一个份额就到循环开始切换到下一个抽水并等待
func EthFeeController(pool *connection.PoolServer) {
	if len(pool.FeeInstance) == 0 {
		return
	}

	startTime := time.Now()
	feeShareNeed := 0
	var feeInfo *connection.FeeStatesClient
	var onlineWorker []*connection.WorkerMiner

	// 高难度的分数设置 5分钟 后超时 难度小就一直循环
feeSelection:
	for true {
		select {
		case <-pool.Context.Done():
			return
		case <-time.After(8 * time.Second):
			// 找出进度最小的
			feeInfo = pool.FeeInstance[0]
			for i := 1; i < len(pool.FeeInstance); i++ {
				if feeInfo.GetFeeProgress() > pool.FeeInstance[i].GetFeeProgress() {
					feeInfo = pool.FeeInstance[i]
				}
			}
			if feeInfo.UpstreamClient.Disconnected {
				logrus.Warnf("[%s][%s][%s][%f] 抽水上游正在重连或者已断线!", feeInfo.PoolServer.Config.Name, feeInfo.Wallet, feeInfo.NamePrefix, feeInfo.Pct)
				continue
			}

			logrus.Debugf("[%s][%s][%s][%f][%fmin] 开始检测抽水，选择抽水配置 [%s]: %f", feeInfo.PoolServer.Config.Name, feeInfo.Wallet, feeInfo.NamePrefix, feeInfo.Pct, time.Since(startTime).Minutes(), feeInfo.NamePrefix, feeInfo.GetFeeProgress())
			feeShareNeed = feeInfo.GetShareDiff()
			logrus.Debugf("[%s][%s][%s][%f] 需要抽取份额数量: %d", feeInfo.PoolServer.Config.Name, feeInfo.Wallet, feeInfo.NamePrefix, feeInfo.Pct, feeShareNeed)

			if feeShareNeed <= 0 {
				continue
			}

			onlineWorker = *feeInfo.PoolServer.GetOnlineWorker()

			// 找这么多的机器来抽水
			// 获取这些占总机器的百分比 太大了就减少
			// 如果要的份额比机器多
			if feeShareNeed > len(onlineWorker) {
				feeShareNeed = len(onlineWorker)
			}

			if len(onlineWorker) <= 0 {
				continue
			}

			// 根据 评分 升序
			sort.SliceStable(onlineWorker, func(i, j int) bool {
				return onlineWorker[i].CalcScore().FinalScore > onlineWorker[j].CalcScore().FinalScore
			})

			logrus.Debugf("[%s][%s][%s][%f] 最终抽取份额数量: %d", feeInfo.PoolServer.Config.Name, feeInfo.Wallet, feeInfo.NamePrefix, feeInfo.Pct, feeShareNeed)
			if feeShareNeed > 0 {
				break feeSelection
			}
		}
	}

	// 分发抽水任务
	wgJob := sync.WaitGroup{}
	wgJob.Add(feeShareNeed)
	for i := 0; i < feeShareNeed; i++ {
		m := onlineWorker[i]

		go func() {
			defer wgJob.Done()

			// 启动抽水监测
			feeWorkerMinersObj, ok := feeInfo.PoolServer.WorkerMinerFeeDB.Load(feeInfo)
			if !ok {
				return
			}
			feeWorkerMiners := feeWorkerMinersObj.(*connection.WorkerMinerSliceWrapper)

			feeShareObj, _ := m.FeeShareIndividual.LoadOrStore(feeInfo, int64(0))
			feeShare := feeShareObj.(int64)

			// 开始抽水
			logrus.Debugf("[%s][%s][%s][%f] 矿机 [%s] 开始抽水 | 分数: [%f]", feeInfo.PoolServer.Config.Name, feeInfo.Wallet, feeInfo.NamePrefix, feeInfo.Pct, m.GetID(), m.CalcScore().FinalScore)

			m.DropUpstream = true
			m.LastFeeTime = time.Now()
			m.LastFeeAtShare = m.TotalShare
			if !feeWorkerMiners.HasMiner(m) {
				feeWorkerMiners.Add(m)
			}

			feeStart := time.Now()

			defer func() {
				m.DropUpstream = false
				feeWorkerMiners.Remove(m)
				logrus.Debugf("[%s][%s][%s][%f] 矿机 [%s] 抽水结束", feeInfo.PoolServer.Config.Name, feeInfo.Wallet, feeInfo.NamePrefix, feeInfo.Pct, m.GetID())
			}()

			for {
				select {
				case <-pool.Context.Done():
					return
				case <-time.After(2 * time.Second):
					// 突然掉线
					if !m.IsOnline() || !feeWorkerMiners.HasMiner(m) {
						logrus.Warnf("[%s][%s][%s][%f] 矿机 [%s] 在抽水的时掉线，取消本次抽水！", feeInfo.PoolServer.Config.Name, feeInfo.Wallet, feeInfo.NamePrefix, feeInfo.Pct, m.GetID())
						return
					}
					if feeInfo.UpstreamClient.Disconnected {
						logrus.Warnf("[%s][%s][%s][%f] 矿机 [%s] 抽水上游正在重连或者已断线，取消本次抽水！", feeInfo.PoolServer.Config.Name, feeInfo.Wallet, feeInfo.NamePrefix, feeInfo.Pct, m.GetID())
						return
					}

					// 如果份额增加了 1 个以上就跳出
					feeShareObjNew, _ := m.FeeShareIndividual.LoadOrStore(feeInfo, int64(0))
					feeShareNew := feeShareObjNew.(int64)
					if feeShareNew-feeShare >= 1 {
						return
					}

					// 如果 45s 还没有份额就停止抽水
					if time.Since(feeStart).Seconds() > 120 {
						// 没抽到就加冷却
						m.LastFeeTime = time.Now().Add(4 * time.Minute)
						return
					}
				}
			}
		}()
	}
	wgJob.Wait()
	feeInfo.FeeCount++
	logrus.Debugf("[%s][%s][%s][%f] 任务结束!", feeInfo.PoolServer.Config.Name, feeInfo.Wallet, feeInfo.NamePrefix, feeInfo.Pct)
}
