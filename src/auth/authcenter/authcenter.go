/*
 * Tencent is pleased to support the open source community by making 蓝鲸 available.
 * Copyright (C) 2017-2019 THL A29 Limited, a Tencent company. All rights reserved.
 * Licensed under the MIT License (the "License"); you may not use this file except
 * in compliance with the License. You may obtain a copy of the License at
 * http://opensource.org/licenses/MIT
 * Unless required by applicable law or agreed to in writing, software distributed under
 * the License is distributed on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
 * either express or implied. See the License for the specific language governing permissions and
 * limitations under the License.
 */

package authcenter

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"configcenter/src/apimachinery/flowctrl"
	"configcenter/src/apimachinery/rest"
	"configcenter/src/apimachinery/util"
	"configcenter/src/auth/authcenter/permit"
	"configcenter/src/auth/meta"
)

const (
	authAppCodeHeaderKey   string = "X-BK-APP-CODE"
	authAppSecretHeaderKey string = "X-BK-APP-SECRET"
	cmdbUser               string = "user"
	cmdbUserID             string = "system"
)

// ParseConfigFromKV returns a new config
func ParseConfigFromKV(prefix string, conifgmap map[string]string) (AuthConfig, error) {
	var cfg AuthConfig

	address, exist := conifgmap[prefix+".address"]
	if !exist {
		return cfg, errors.New(`missing "address" configuration for auth center`)
	}

	cfg.Address = strings.Split(strings.Replace(address, " ", "", -1), ",")
	if len(cfg.Address) == 0 {
		return cfg, errors.New(`invalid "address" configuration for auth center`)
	}

	cfg.AppSecret, exist = conifgmap[prefix+".appSecret"]
	if !exist {
		return cfg, errors.New(`missing "appSecret" configuration for auth center`)
	}

	if len(cfg.AppSecret) == 0 {
		return cfg, errors.New(`invalid "appSecret" configuration for auth center`)
	}

	cfg.AppCode, exist = conifgmap[prefix+".appCode"]
	if !exist {
		return cfg, errors.New(`missing "appCode" configuration for auth center`)
	}

	if len(cfg.AppCode) == 0 {
		return cfg, errors.New(`invalid "appCode" configuration for auth center`)
	}

	cfg.SystemID, exist = conifgmap[prefix+".systemID"]
	if !exist {
		return cfg, errors.New(`missing "systemID" configuration for auth center`)
	}

	if len(cfg.SystemID) == 0 {
		return cfg, errors.New(`invalid "systemID" configuration for auth center`)
	}
	return cfg, nil
}

// NewAuthCenter create a instance to handle resources with blueking's AuthCenter.
func NewAuthCenter(tls *util.TLSClientConfig, cfg AuthConfig) (*AuthCenter, error) {
	client, err := util.NewClient(tls)
	if err != nil {
		return nil, err
	}

	c := &util.Capability{
		Client: client,
		Discover: &acDiscovery{
			servers: cfg.Address,
		},
		Throttle: flowctrl.NewRateLimiter(1000, 1000),
		Mock: util.MockInfo{
			Mocked: false,
		},
	}

	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("Accept", "application/json")
	header.Set(authAppCodeHeaderKey, cfg.AppCode)
	header.Set(authAppSecretHeaderKey, cfg.AppSecret)

	return &AuthCenter{
		authClient: &authClient{
			client:      rest.NewRESTClient(c, ""),
			Config:      cfg,
			basicHeader: header,
		},
	}, nil
}

// AuthCenter means BlueKing's authorize center,
// which is also a open source product.
type AuthCenter struct {
	Config AuthConfig
	// http client instance
	client rest.ClientInterface
	// http header info
	header     http.Header
	authClient *authClient
}

func (ac *AuthCenter) Authorize(ctx context.Context, a *meta.AuthAttribute) (decision meta.Decision, err error) {
	resources := make([]meta.ResourceAttribute, 0)
	for _, rsc := range a.Resources {
		// check whether this request is in whitelist, so that it can be skip directly.
		if !permit.IsPermit(&rsc) {
			resources = append(resources, rsc)
		}
	}

	if len(resources) == 0 {
		// no resources need to be authorized.
		return meta.Decision{Authorized: true}, nil
	}

	// there still have resource need to be authorized.
	// so, update the resources.
	a.Resources = resources

	info := &AuthBatch{
		Principal: Principal{
			Type: "user",
			ID:   a.User.UserName,
		},
	}

	// TODO: this operation may be wrong, because some api filters does not
	// fill the business id field, so these api should be normalized.
	if a.BusinessID != 0 {
		info.ScopeType = "biz"
		info.ScopeID = strconv.FormatInt(a.BusinessID, 10)
	} else {
		info.ScopeType = "system"
		info.ScopeID = "bk_cmdb"
	}

	info.ResourceActions = make([]ResourceAction, 0)
	for _, rsc := range a.Resources {

		rscInfo, err := adaptor(&rsc)
		if err != nil {
			return meta.Decision{}, fmt.Errorf("adaptor resource info failed, err: %v", err)
		}

		actionID, err := adaptorAction(&rsc)
		if err != nil {
			return meta.Decision{}, fmt.Errorf("adaptor action failed, err: %v", err)
		}

		info.ResourceActions = append(info.ResourceActions, ResourceAction{
			ActionID:     actionID,
			ResourceInfo: *rscInfo,
		})
	}

	header := http.Header{}
	header.Set(AuthSupplierAccountHeaderKey, a.User.SupplierID)
	return ac.authClient.verifyInList(ctx, header, info)

}

func (ac *AuthCenter) RegisterResource(ctx context.Context, rs ...meta.ResourceAttribute) error {
	for _, r := range rs {
		if len(r.Basic.Type) == 0 {
			return errors.New("invalid resource attribute with empty object")
		}
		scope, err := ac.getScopeInfo(&r)
		if err != nil {
			return err
		}

		// rscInfo, err := adaptor(&r)
		// if err != nil {
		// 	return fmt.Errorf("adaptor resource info failed, err: %v", err)
		// }
		info := &RegisterInfo{
			CreatorType: cmdbUser,
			CreatorID:   cmdbUserID,
			ScopeInfo:   *scope,
			// ResourceInfo: *rscInfo,
		}

		header := http.Header{}
		header.Set(AuthSupplierAccountHeaderKey, r.SupplierAccount)
		return ac.authClient.registerResource(ctx, header, info)
	}

	return nil
}

func (ac *AuthCenter) DeregisterResource(ctx context.Context, rs ...meta.ResourceAttribute) error {
	for _, r := range rs {

		if len(r.Basic.Type) == 0 {
			return errors.New("invalid resource attribute with empty object")
		}

		scope, err := ac.getScopeInfo(&r)
		if err != nil {
			return err
		}

		rscInfo, err := adaptor(&r)
		if err != nil {
			return fmt.Errorf("adaptor resource info failed, err: %v", err)
		}

		info := &DeregisterInfo{
			ScopeInfo:    *scope,
			ResourceInfo: *rscInfo,
		}

		header := http.Header{}
		header.Set(AuthSupplierAccountHeaderKey, r.SupplierAccount)
		return ac.authClient.deregisterResource(ctx, header, info)
	}

	return nil
}

func (ac *AuthCenter) UpdateResource(ctx context.Context, r *meta.ResourceAttribute) error {
	if len(r.Basic.Type) == 0 || len(r.Basic.Name) == 0 {
		return errors.New("invalid resource attribute with empty object or object name")
	}

	scope, err := ac.getScopeInfo(r)
	if err != nil {
		return err
	}

	rscInfo, err := adaptor(r)
	if err != nil {
		return fmt.Errorf("adaptor resource info failed, err: %v", err)
	}
	info := &UpdateInfo{
		ScopeInfo:    *scope,
		ResourceInfo: *rscInfo,
	}

	header := http.Header{}
	header.Set(AuthSupplierAccountHeaderKey, r.SupplierAccount)
	return ac.authClient.updateResource(ctx, header, info)
}

func (ac *AuthCenter) Get(ctx context.Context) error {
	panic("implement me")
}

func (ac *AuthCenter) getScopeInfo(r *meta.ResourceAttribute) (*ScopeInfo, error) {
	s := new(ScopeInfo)
	// TODO: this operation may be wrong, because some api filters does not
	// fill the business id field, so these api should be normalized.
	if r.BusinessID != 0 {
		s.ScopeType = "biz"
		s.ScopeID = strconv.FormatInt(r.BusinessID, 10)
	} else {
		s.ScopeType = "system"
		s.ScopeID = "bk-cmdb"
	}
	return s, nil
}

type acDiscovery struct {
	// auth's servers address, must prefixed with http:// or https://
	servers []string
	index   int
	sync.Mutex
}

func (s *acDiscovery) GetServers() ([]string, error) {
	s.Lock()
	defer s.Unlock()

	num := len(s.servers)
	if num == 0 {
		return []string{}, errors.New("oops, there is no server can be used")
	}

	if s.index < num-1 {
		s.index = s.index + 1
		return append(s.servers[s.index-1:], s.servers[:s.index-1]...), nil
	} else {
		s.index = 0
		return append(s.servers[num-1:], s.servers[:num-1]...), nil
	}
}
