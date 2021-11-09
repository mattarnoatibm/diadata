package tradesBlockService

import (
	"errors"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/cnf/structhash"
	"github.com/diadata-org/diadata/pkg/dia"
	models "github.com/diadata-org/diadata/pkg/model"
	"github.com/sirupsen/logrus"
)

type nothing struct{}

var log *logrus.Logger

func init() {
	log = logrus.New()
}

var (
	stablecoins = map[string]interface{}{
		"USDC": "",
		"USDT": "",
		"TUSD": "",
		"DAI":  "",
		"PAX":  "",
		"BUSD": "",
	}
	tol = float64(0.1)
)

type TradesBlockService struct {
	shutdown        chan nothing
	shutdownDone    chan nothing
	chanTrades      chan *dia.Trade
	chanTradesBlock chan *dia.TradesBlock
	errorLock       sync.RWMutex
	error           error
	closed          bool
	started         bool
	BlockDuration   int64
	currentBlock    *dia.TradesBlock
	priceCache      map[dia.Asset]float64
	datastore       models.Datastore
	historical      bool
}

func NewTradesBlockService(datastore models.Datastore, blockDuration int64, historical bool) *TradesBlockService {
	s := &TradesBlockService{
		shutdown:        make(chan nothing),
		shutdownDone:    make(chan nothing),
		chanTrades:      make(chan *dia.Trade),
		chanTradesBlock: make(chan *dia.TradesBlock),
		error:           nil,
		started:         false,
		currentBlock:    nil,
		BlockDuration:   blockDuration,
		priceCache:      make(map[dia.Asset]float64),
		datastore:       datastore,
		historical:      historical,
	}
	go s.mainLoop()
	return s
}

// runs in a goroutine until s is closed
func (s *TradesBlockService) mainLoop() {
	for {
		select {
		case <-s.shutdown:
			log.Println("TradesBlockService shutting down")
			s.cleanup(nil)
			return
		case t := <-s.chanTrades:
			s.process(*t)
		}
	}
}

func (s *TradesBlockService) process(t dia.Trade) {
	tInit := time.Now()

	var verifiedTrade bool
	// baseTokenSymbol := t.GetBaseToken()

	tInitMain := time.Now()
	// Price estimation can only be done for verified pairs.
	// Trades with unverified pairs are still saved, but not sent to the filtersBlockService.
	if t.VerifiedPair {
		if t.BaseToken.Address == "840" && t.BaseToken.Blockchain == dia.FIAT {
			// All prices are measured in US-Dollar, so just price for base token == USD
			t.EstimatedUSDPrice = t.Price
			verifiedTrade = true
		} else {
			// Get price of base token.
			// This can be switched to GetAssetPriceUSD(asset, timestamp) when switching to historical scrapers.
			// val, err := s.datastore.GetAssetPriceUSDCache(t.BaseToken)
			var quotation *models.AssetQuotation
			var price float64
			var ok bool
			var err error
			if s.historical {
				// Look for historic price of base token at trade time...
				price, err = s.datastore.GetAssetPriceUSD(t.BaseToken, t.Time)
			} else {
				// ...or latest price. This method is quicker as it first queries the cache.
				// Comment Philipp 09/11/2021: This might still be too slow, as it queries influx
				// as soon as there is no quotation in the cache.
				// price, err = s.datastore.GetAssetPriceUSDLatest(t.BaseToken)
				tInitCaching := time.Now()
				if _, ok = s.priceCache[t.BaseToken]; ok {
					price = s.priceCache[t.BaseToken]
					log.Infof("quotation for %s from local cache: %v", t.BaseToken.Symbol, price)
				} else {
					quotation, err = s.datastore.GetAssetQuotationCache(t.BaseToken)
					price = quotation.Price
					s.priceCache[t.BaseToken] = price
					log.Infof("quotation for %s from redis cache: %v", t.BaseToken.Symbol, price)
				}

				log.Info("time spent for getting price of base token: ", time.Since(tInitCaching))
			}
			if err != nil {
				log.Errorf("Cannot use trade %s. Can't find quotation for base token.", t.Pair)
			} else {
				if price > 0.0 {
					// log.Infof("price of trade %s on exchange %s: %v", t.Pair, t.Source, t.Price)
					// log.Info("price of base token: ", price)
					// log.Info("resulting estimatedUSDPrice: ", t.Price*price)
					// TO DO: Switch to big.Float?
					t.EstimatedUSDPrice = t.Price * price
					if t.EstimatedUSDPrice > 0 {
						verifiedTrade = true
					}
				}
			}
		}
	}
	log.Info("time spent for tInitMain: ", time.Since(tInitMain))

	// // If estimated price for stablecoin diverges too much ignore trade
	if _, ok := stablecoins[t.Symbol]; ok {
		if math.Abs(t.EstimatedUSDPrice-1) > tol {
			log.Errorf("price for stablecoin %s diverges by %v", t.Symbol, math.Abs(t.EstimatedUSDPrice-1))
			verifiedTrade = false
		}
	}
	// Comment Philipp: We could make another check here. Store CG and/or CMC quotation in redis cache
	// and compare with estimatedUSDPrice. If deviation is too large ignore trade. If we do so,
	// we should already think about how to do it best with regards to historic values, as these are coming up.

	err := s.datastore.SaveTradeInflux(&t)
	if err != nil {
		log.Error(err)
	}

	if s.currentBlock != nil && s.currentBlock.TradesBlockData.BeginTime.After(t.Time) {
		log.Debugf("ignore trade should be in previous block %v", t)
		verifiedTrade = false
	}

	// Only verified trades of verified pairs with nonzero price are added to the tradesBlock
	if verifiedTrade && t.EstimatedUSDPrice > 0 {
		if s.currentBlock == nil || s.currentBlock.TradesBlockData.EndTime.Before(t.Time) {
			if s.currentBlock != nil {
				t0 := time.Now()
				s.finaliseCurrentBlock()
				log.Info("time spent for finalizing current block: ", time.Since(t0))
			}

			b := &dia.TradesBlock{
				TradesBlockData: dia.TradesBlockData{
					Trades:    []dia.Trade{},
					EndTime:   time.Unix((t.Time.Unix()/s.BlockDuration)*s.BlockDuration+s.BlockDuration, 0),
					BeginTime: time.Unix((t.Time.Unix()/s.BlockDuration)*s.BlockDuration, 0),
				},
			}
			if s.currentBlock != nil {
				log.Info("created new block beginTime:", b.TradesBlockData.BeginTime, "previous block nb trades:", len(s.currentBlock.TradesBlockData.Trades))
			}
			s.currentBlock = b
			s.priceCache = make(map[dia.Asset]float64)
			err = s.datastore.Flush()
			if err != nil {
				log.Error(err)
			}
		}
		s.currentBlock.TradesBlockData.Trades = append(s.currentBlock.TradesBlockData.Trades, t)
	} else {
		log.Debugf("ignore trade  %v", t)
	}
	log.Info("time spent for process: ", time.Since(tInit))
}

func (s *TradesBlockService) finaliseCurrentBlock() {

	sort.Slice(s.currentBlock.TradesBlockData.Trades, func(i, j int) bool {
		return s.currentBlock.TradesBlockData.Trades[i].Time.Before(s.currentBlock.TradesBlockData.Trades[j].Time)
	})

	hash, err := structhash.Hash(s.currentBlock.TradesBlockData, 1)
	if err != nil {
		log.Printf("error on hash")
		hash = "hashError"
	}
	s.currentBlock.BlockHash = hash
	s.currentBlock.TradesBlockData.TradesNumber = len(s.currentBlock.TradesBlockData.Trades)
	s.chanTradesBlock <- s.currentBlock
}

func (s *TradesBlockService) ProcessTrade(trade *dia.Trade) {
	s.chanTrades <- trade
}

func (s *TradesBlockService) Close() error {
	if s.closed {
		return errors.New("TradesBlockService: Already closed")
	}
	close(s.shutdown)
	<-s.shutdownDone
	return s.error
}

// must only be called from mainLoop
func (s *TradesBlockService) cleanup(err error) {
	s.errorLock.Lock()
	defer s.errorLock.Unlock()
	if err != nil {
		s.error = err
	}
	s.closed = true
	close(s.shutdownDone) // signal that shutdown is complete
}

func (s *TradesBlockService) Channel() chan *dia.TradesBlock {
	return s.chanTradesBlock
}
