/*
-------------------------------------------------
   Author :       Zhang Fan
   date：         2020/3/11
   Description :
-------------------------------------------------
*/

package zbec

import (
    "context"
    "errors"
    "reflect"
    "sync"
    "time"

    "github.com/zlyuancn/zerrors"
    "github.com/zlyuancn/zlog2"
    "github.com/zlyuancn/zsingleflight"
)

var ErrNoEntry = errors.New("条目不存在")
var ErrLoaderFnNotExists = errors.New("db加载函数不存在或为空")

// 表示缓存数据库字节长度为0, 或者db加载结果为nil
var NilData = errors.New("空数据")

// 默认本地缓存有效时间
const DefaultLocalCacheExpire = time.Second

type BECache struct {
    cdb ICacheDB // 缓存数据库

    local_cdb    ICacheDB      // 本地缓存
    local_cdb_ex time.Duration // 本地缓存有效时间

    sf      *zsingleflight.SingleFlight // 单飞
    loaders map[string]ILoader          // 加载器配置
    mx      sync.RWMutex                // 对注册的加载器加锁
    log     ILoger                      // 日志组件

    cache_nil bool // 是否缓存空数据
}

func New(c ICacheDB, opts ...Option) *BECache {
    m := &BECache{
        cdb: c,

        sf:      zsingleflight.New(),
        loaders: make(map[string]ILoader),
        log:     zlog2.DefaultLogger,

        cache_nil: true,
    }

    for _, o := range opts {
        o(m)
    }

    return m
}

// 设置, 仅用于初始化设置, 正式使用时不应该再调用这个方法
func (m *BECache) SetOptions(opts ...Option) {
    for _, o := range opts {
        o(m)
    }
}

// 为空间注册加载器, 空间名为加载器名, 已注册的空间会被新的加载器替换掉
func (m *BECache) RegisterLoader(loader ILoader) {
    m.mx.Lock()
    m.loaders[loader.Name()] = loader
    m.mx.Unlock()
}

// 获取加载器
func (m *BECache) getLoader(space string) ILoader {
    m.mx.RLock()
    s := m.loaders[space]
    m.mx.RUnlock()
    return s
}

func (m *BECache) cacheGet(query *Query, a interface{}) (interface{}, error) {
    if m.local_cdb != nil {
        out, err := m.local_cdb.Get(query, a)
        if err == nil || err == NilData {
            return out, err
        }
    }

    out, err := m.cdb.Get(query, a)
    if err == nil {
        m.localCacheSet(query, out)
        return out, nil
    }
    if err == NilData {
        m.localCacheSet(query, nil)
        return nil, NilData
    }
    if err == ErrNoEntry {
        return out, err
    }
    return nil, zerrors.WithMessage(err, "缓存加载失败")
}
func (m *BECache) cacheSet(query *Query, a interface{}, loader ILoader) {
    m.localCacheSet(query, a)
    if a == nil && !m.cache_nil {
        return
    }

    if e := m.cdb.Set(query, a, loader.Expire()); e != nil {
        m.log.Warn(zerrors.WithMessagef(e, "缓存失败<%s>", query.FullPath()))
    }
}
func (m *BECache) cacheDel(query *Query) error {
    if m.local_cdb != nil {
        _ = m.local_cdb.Del(query)
    }
    return m.cdb.Del(query)
}
func (m *BECache) cacheDelSpace(space string) error {
    if m.local_cdb != nil {
        _ = m.local_cdb.DelSpaceData(space)
    }
    return m.cdb.DelSpaceData(space)
}
func (m *BECache) localCacheSet(query *Query, a interface{}) {
    if m.local_cdb != nil {
        _ = m.local_cdb.Set(query, a, m.local_cdb_ex)
    }
}

// 从db加载
func (m *BECache) loadDB(query *Query, loader ILoader) (interface{}, error) {
    // 从db加载
    a, err := loader.Load(query)
    if err != nil {
        if e := m.cdb.Del(query); e != nil { // 从db加载失败时从缓存删除
            m.log.Warn(zerrors.WithMessagef(e, "db加载失败后删除缓存失败<%s>", query.FullPath()))
        }
        return nil, zerrors.WithMessage(err, "db加载失败")
    }

    // 缓存
    m.cacheSet(query, a, loader)
    return a, nil
}

// 获取数据, 空间必须已注册加载器
func (m *BECache) Get(query *Query, a interface{}) error {
    return m.GetWithContext(nil, query, a)
}

// 获取数据, 空间必须已注册加载器
func (m *BECache) GetWithContext(ctx context.Context, query *Query, a interface{}) error {
    query.Check()
    space := m.getLoader(query.Space)
    if space == nil {
        return zerrors.NewSimplef("空间未注册加载器 <%s>", query.Space)
    }

    return m.GetWithLoader(ctx, query, a, space)
}

// 获取数据, 缓存数据不存在时使用指定加载器获取数据
func (m *BECache) GetWithLoader(ctx context.Context, query *Query, a interface{}, loader ILoader) (err error) {
    query.Check()
    if ctx == nil {
        return m.getWithLoader(query, a, loader)
    }

    done := make(chan struct{})
    go func() {
        err = m.getWithLoader(query, a, loader)
        done <- struct{}{}
    }()

    select {
    case <-done:
        return err
    case <-ctx.Done():
        return ctx.Err()
    }
}

func (m *BECache) getWithLoader(query *Query, a interface{}, loader ILoader) error {
    // 同时只能有一个goroutine在获取数据,其它goroutine直接等待结果
    out, err := m.sf.Do(query.FullPath(), func() (interface{}, error) {
        return m.query(query, a, loader)
    })

    if err != nil {
        if err == NilData {
            return NilData
        }
        return zerrors.WithMessagef(err, "加载失败<%s>", query.FullPath())
    }

    if out == nil {
        return NilData
    }

    // todo: 可以考虑进一步优化, 因为 src 是重复执行
    reflect.ValueOf(a).Elem().Set(reflect.Indirect(reflect.ValueOf(out)))
    return nil
}

func (m *BECache) query(query *Query, a interface{}, loader ILoader) (interface{}, error) {
    out, gerr := m.cacheGet(query, a)
    if gerr == nil || gerr == NilData {
        return out, gerr
    }

    out, lerr := m.loadDB(query, loader)
    if lerr == nil || lerr == NilData {
        return out, lerr
    }

    if gerr != ErrNoEntry {
        return nil, zerrors.WithMessage(gerr, lerr.Error())
    }
    return nil, lerr
}

// 删除指定数据
func (m *BECache) DelData(query *Query) error {
    query.Check()
    return m.cacheDel(query)
}

// 删除空间数据
func (m *BECache) DelSpaceData(space string) error {
    return m.cacheDelSpace(space)
}
