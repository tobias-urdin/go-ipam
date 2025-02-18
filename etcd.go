package ipam

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"go.etcd.io/etcd/client/pkg/v3/transport"
	clientv3 "go.etcd.io/etcd/client/v3"
)

type etcd struct {
	keyPrefix string
	etcdDB *clientv3.Client
	lock   sync.RWMutex
}

// NewEtcd create a etcd storage for ipam
func NewEtcd(ip, port, cert, key, ca string, insecureskip bool, keyPrefix string) Storage {
	return newEtcd(ip, port, cert, key, ca, insecureskip, keyPrefix)
}

func (e *etcd) Name() string {
	return "etcd"
}

func newEtcd(ip, port, cert, key, ca string, insecureskip bool, keyPrefix string) *etcd {
	tlsInfo := transport.TLSInfo{
		CertFile:           cert,
		KeyFile:            key,
		TrustedCAFile:      ca,
		// nolint:gosec
		// #nosec G402
		InsecureSkipVerify: insecureskip,
	}

	tlsConfig, err := tlsInfo.ClientConfig()
	if err != nil {
		log.Fatal(err)
	}

	etcdConfig := clientv3.Config{
		Endpoints:   []string{fmt.Sprintf("%s:%s", ip, port)},
		DialTimeout: 5 * time.Second,
		Context:     context.Background(),
                TLS:         tlsConfig,
	}

	cli, err := clientv3.New(etcdConfig)
	if err != nil {
		log.Fatal(err)
	}

	return &etcd{
		keyPrefix: keyPrefix,
		etcdDB: cli,
	}
}

func (e *etcd) CreatePrefix(ctx context.Context, prefix Prefix) (Prefix, error) {
	e.lock.Lock()
	defer e.lock.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	get, err := e.etcdDB.Get(ctx, e.keyPrefix + prefix.Cidr)
	defer cancel()
	if err != nil {
		return Prefix{}, fmt.Errorf("unable to read existing prefix:%v, error:%w", prefix, err)
	}

	if get.Count != 0 {
		return Prefix{}, fmt.Errorf("prefix already exists:%v", prefix)
	}

	pfx, err := prefix.toJSON()
	if err != nil {
		return Prefix{}, err
	}
	ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
	_, err = e.etcdDB.Put(ctx, e.keyPrefix + prefix.Cidr, string(pfx))
	defer cancel()
	if err != nil {
		return Prefix{}, fmt.Errorf("unable to create prefix:%v, error:%w", prefix, err)
	}

	return prefix, nil
}

func (e *etcd) ReadPrefix(ctx context.Context, prefix string) (Prefix, error) {
	e.lock.Lock()
	defer e.lock.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	get, err := e.etcdDB.Get(ctx, e.keyPrefix + prefix)
	defer cancel()
	if err != nil {
		return Prefix{}, fmt.Errorf("unable to read data from ETCD error:%w", err)
	}

	if get.Count == 0 {
		return Prefix{}, fmt.Errorf("unable to read existing prefix:%v, error:%w", prefix, err)
	}

	return fromJSON(get.Kvs[0].Value)
}

func (e *etcd) DeleteAllPrefixes(ctx context.Context) error {
	e.lock.RLock()
	defer e.lock.RUnlock()
	ctx, cancel := context.WithTimeout(ctx, 50*time.Minute)
	defaultOpts := []clientv3.OpOption{clientv3.WithPrefix(), clientv3.WithKeysOnly(), clientv3.WithSerializable()}
	pfxs, err := e.etcdDB.Get(ctx, e.keyPrefix, defaultOpts...)
	defer cancel()
	if err != nil {
		return fmt.Errorf("unable to get all prefix cidrs:%w", err)
	}

	for _, pfx := range pfxs.Kvs {
		_, err := e.etcdDB.Delete(ctx, e.keyPrefix + string(pfx.Key))
		if err != nil {
			return fmt.Errorf("unable to delete prefix:%w", err)
		}
	}
	return nil
}

func (e *etcd) ReadAllPrefixes(ctx context.Context) (Prefixes, error) {
	e.lock.Lock()
	defer e.lock.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defaultOpts := []clientv3.OpOption{clientv3.WithPrefix(), clientv3.WithKeysOnly(), clientv3.WithSerializable()}
	pfxs, err := e.etcdDB.Get(ctx, e.keyPrefix, defaultOpts...)
	defer cancel()
	if err != nil {
		return nil, fmt.Errorf("unable to get all prefix cidrs:%w", err)
	}

	result := Prefixes{}
	for _, pfx := range pfxs.Kvs {
		v, err := e.etcdDB.Get(ctx, e.keyPrefix + string(pfx.Key))
		if err != nil {
			return nil, err
		}
		pfx, err := fromJSON(v.Kvs[0].Value)
		if err != nil {
			return nil, err
		}
		result = append(result, pfx)
	}
	return result, nil
}
func (e *etcd) ReadAllPrefixCidrs(ctx context.Context) ([]string, error) {
	e.lock.Lock()
	defer e.lock.Unlock()

	allPrefix := []string{}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defaultOpts := []clientv3.OpOption{clientv3.WithPrefix(), clientv3.WithKeysOnly(), clientv3.WithSerializable()}
	pfxs, err := e.etcdDB.Get(ctx, e.keyPrefix, defaultOpts...)
	defer cancel()
	if err != nil {
		return nil, fmt.Errorf("unable to get all prefix cidrs:%w", err)
	}

	for _, pfx := range pfxs.Kvs {
		allPrefix = append(allPrefix, string(pfx.Key))
	}

	return allPrefix, nil
}
func (e *etcd) UpdatePrefix(ctx context.Context, prefix Prefix) (Prefix, error) {
	e.lock.Lock()
	defer e.lock.Unlock()

	oldVersion := prefix.version
	prefix.version = oldVersion + 1
	pn, err := prefix.toJSON()
	if err != nil {
		return Prefix{}, err
	}

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	p, err := e.etcdDB.Get(ctx, e.keyPrefix + prefix.Cidr)
	defer cancel()
	if err != nil {
		return Prefix{}, fmt.Errorf("unable to read cidrs from ETCD:%w", err)
	}

	if p.Count == 0 {
		return Prefix{}, fmt.Errorf("unable to get all prefix cidrs:%w", err)
	}

	oldPrefix, err := fromJSON([]byte(p.Kvs[0].Value))
	if err != nil {
		return Prefix{}, err
	}

	// Actual operation (local in optimistic lock).
	if oldPrefix.version != oldVersion {
		return Prefix{}, fmt.Errorf("%w: unable to update prefix:%s", ErrOptimisticLockError, prefix.Cidr)
	}

	// Operation is committed only if the watched keys remain unchanged.
	ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
	_, err = e.etcdDB.Put(ctx, e.keyPrefix + prefix.Cidr, string(pn))
	defer cancel()
	if err != nil {
		return Prefix{}, fmt.Errorf("unable to update prefix:%s, error:%w", prefix.Cidr, err)
	}

	return prefix, nil
}
func (e *etcd) DeletePrefix(ctx context.Context, prefix Prefix) (Prefix, error) {
	e.lock.Lock()
	defer e.lock.Unlock()

	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_, err := e.etcdDB.Delete(ctx, e.keyPrefix + prefix.Cidr)
	defer cancel()
	if err != nil {
		return *prefix.deepCopy(), err
	}
	return *prefix.deepCopy(), nil
}
