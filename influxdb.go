package influxdb

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
)

const (
	UDPMaxMessageSize = 2048
)

type Client struct {
	host        string
	username    string
	password    string
	database    string
	httpClient  *http.Client
	udpConn     *net.UDPConn
	schema      string
	compression bool
}

type ClientConfig struct {
	Host       string
	Username   string
	Password   string
	Database   string
	HttpClient *http.Client
	IsSecure   bool
	IsUDP      bool
}

var defaults *ClientConfig

func init() {
	defaults = &ClientConfig{
		Host:       "localhost:8086",
		Username:   "root",
		Password:   "root",
		Database:   "",
		IsUDP:      false,
		HttpClient: http.DefaultClient,
	}
}

func getDefault(value, defaultValue string) string {
	if value == "" {
		return defaultValue
	}
	return value
}

func NewClient(config *ClientConfig) (*Client, error) {
	host := getDefault(config.Host, defaults.Host)
	username := getDefault(config.Username, defaults.Username)
	password := getDefault(config.Password, defaults.Password)
	database := getDefault(config.Database, defaults.Database)
	if config.HttpClient == nil {
		config.HttpClient = defaults.HttpClient
	}
	var udpConn *net.UDPConn
	if config.IsUDP {
		serverAddr, err := net.ResolveUDPAddr("udp", host)
		if err != nil {
			return nil, err
		}
		udpConn, err = net.DialUDP("udp", nil, serverAddr)
		if err != nil {
			return nil, err
		}
	}

	schema := "http"
	if config.IsSecure {
		schema = "https"
	}
	return &Client{host, username, password, database, config.HttpClient, udpConn, schema, true}, nil
}

func (self *Client) DisableCompression() {
	self.compression = false
}

func (self *Client) getUrl(path string) string {
	return self.getUrlWithUserAndPass(path, self.username, self.password)
}

func (self *Client) getUrlWithUserAndPass(path, username, password string) string {
	return fmt.Sprintf("%s://%s%s?u=%s&p=%s", self.schema, self.host, path, username, password)
}

func responseToError(response *http.Response, err error, closeResponse bool) error {
	if err != nil {
		return err
	}
	if closeResponse {
		defer response.Body.Close()
	}
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	defer response.Body.Close()
	body, err := ioutil.ReadAll(response.Body)
	if err != nil {
		return err
	}
	return fmt.Errorf("Server returned (%d): %s", response.StatusCode, string(body))
}

func (self *Client) CreateDatabase(name string) error {
	url := self.getUrl("/db")
	payload := map[string]string{"name": name}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := self.httpClient.Post(url, "application/json", bytes.NewBuffer(data))
	return responseToError(resp, err, true)
}

func (self *Client) del(url string) (*http.Response, error) {
	return self.delWithBody(url, nil)
}

func (self *Client) delWithBody(url string, body io.Reader) (*http.Response, error) {
	req, err := http.NewRequest("DELETE", url, body)
	if err != nil {
		return nil, err
	}
	return self.httpClient.Do(req)
}

func (self *Client) DeleteDatabase(name string) error {
	url := self.getUrl("/db/" + name)
	resp, err := self.del(url)
	return responseToError(resp, err, true)
}

func (self *Client) get(url string) ([]byte, error) {
	resp, err := self.httpClient.Get(url)
	err = responseToError(resp, err, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)
	return body, err
}

func (self *Client) listSomething(url string) ([]map[string]interface{}, error) {
	body, err := self.get(url)
	if err != nil {
		return nil, err
	}
	somethings := []map[string]interface{}{}
	err = json.Unmarshal(body, &somethings)
	if err != nil {
		return nil, err
	}
	return somethings, nil
}

func (self *Client) GetDatabaseList() ([]map[string]interface{}, error) {
	url := self.getUrl("/db")
	return self.listSomething(url)
}

func (self *Client) CreateClusterAdmin(name, password string) error {
	url := self.getUrl("/cluster_admins")
	payload := map[string]string{"name": name, "password": password}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := self.httpClient.Post(url, "application/json", bytes.NewBuffer(data))
	return responseToError(resp, err, true)
}

func (self *Client) UpdateClusterAdmin(name, password string) error {
	url := self.getUrl("/cluster_admins/" + name)
	payload := map[string]string{"password": password}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := self.httpClient.Post(url, "application/json", bytes.NewBuffer(data))
	return responseToError(resp, err, true)
}

func (self *Client) DeleteClusterAdmin(name string) error {
	url := self.getUrl("/cluster_admins/" + name)
	resp, err := self.del(url)
	return responseToError(resp, err, true)
}

func (self *Client) GetClusterAdminList() ([]map[string]interface{}, error) {
	url := self.getUrl("/cluster_admins")
	return self.listSomething(url)
}

func (self *Client) Servers() ([]map[string]interface{}, error) {
	url := self.getUrl("/cluster/servers")
	return self.listSomething(url)
}

func (self *Client) RemoveServer(id int) error {
	resp, err := self.del(self.getUrl(fmt.Sprintf("/cluster/servers/%d", id)))
	return responseToError(resp, err, true)
}

// Creates a new database user for the given database. permissions can
// be omitted in which case the user will be able to read and write to
// all time series. If provided, there should be two strings, the
// first for read and the second for write. The strings are regexes
// that are used to match the time series name to determine whether
// the user has the ability to read/write to the given time series.
//
//     client.CreateDatabaseUser("db", "user", "pass")
//     // the following user cannot read from any series and can write
//     // to the limited time series only
//     client.CreateDatabaseUser("db", "limited", "pass", "^$", "limited")
func (self *Client) CreateDatabaseUser(database, name, password string, permissions ...string) error {
	readMatcher, writeMatcher := ".*", ".*"
	switch len(permissions) {
	case 0:
	case 2:
		readMatcher, writeMatcher = permissions[0], permissions[1]
	default:
		return fmt.Errorf("You have to provide two ")
	}

	url := self.getUrl("/db/" + database + "/users")
	payload := map[string]string{"name": name, "password": password, "readFrom": readMatcher, "writeTo": writeMatcher}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := self.httpClient.Post(url, "application/json", bytes.NewBuffer(data))
	return responseToError(resp, err, true)
}

// Change the cluster admin password
func (self *Client) ChangeClusterAdminPassword(name, newPassword string) error {
	url := self.getUrl("/cluster_admins/" + name)
	payload := map[string]interface{}{"password": newPassword}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := self.httpClient.Post(url, "application/json", bytes.NewBuffer(data))
	return responseToError(resp, err, true)
}

// Change the user password, adming flag and optionally permissions
func (self *Client) ChangeDatabaseUser(database, name, newPassword string, isAdmin bool, newPermissions ...string) error {
	switch len(newPermissions) {
	case 0, 2:
	default:
		return fmt.Errorf("You have to provide two ")
	}

	url := self.getUrl("/db/" + database + "/users/" + name)
	payload := map[string]interface{}{"password": newPassword, "admin": isAdmin}
	if len(newPermissions) == 2 {
		payload["readFrom"] = newPermissions[0]
		payload["writeTo"] = newPermissions[1]
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := self.httpClient.Post(url, "application/json", bytes.NewBuffer(data))
	return responseToError(resp, err, true)
}

// See Client.CreateDatabaseUser for more info on the permissions
// argument
func (self *Client) updateDatabaseUserCommon(database, name string, password *string, isAdmin *bool, permissions ...string) error {
	url := self.getUrl("/db/" + database + "/users/" + name)
	payload := map[string]interface{}{}
	if password != nil {
		payload["password"] = *password
	}
	if isAdmin != nil {
		payload["admin"] = *isAdmin
	}
	switch len(permissions) {
	case 0:
	case 2:
		payload["readFrom"] = permissions[0]
		payload["writeTo"] = permissions[1]
	default:
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	resp, err := self.httpClient.Post(url, "application/json", bytes.NewBuffer(data))
	return responseToError(resp, err, true)
}

func (self *Client) UpdateDatabaseUser(database, name, password string) error {
	return self.updateDatabaseUserCommon(database, name, &password, nil)
}

func (self *Client) UpdateDatabaseUserPermissions(database, name, readPermission, writePermissions string) error {
	return self.updateDatabaseUserCommon(database, name, nil, nil, readPermission, writePermissions)
}

func (self *Client) DeleteDatabaseUser(database, name string) error {
	url := self.getUrl("/db/" + database + "/users/" + name)
	resp, err := self.del(url)
	return responseToError(resp, err, true)
}

func (self *Client) GetDatabaseUserList(database string) ([]map[string]interface{}, error) {
	url := self.getUrl("/db/" + database + "/users")
	return self.listSomething(url)
}

func (self *Client) AlterDatabasePrivilege(database, name string, isAdmin bool, permissions ...string) error {
	return self.updateDatabaseUserCommon(database, name, nil, &isAdmin, permissions...)
}

type TimePrecision string

const (
	Second      TimePrecision = "s"
	Millisecond TimePrecision = "m"
	Microsecond TimePrecision = "u"
)

func (self *Client) WriteSeries(series []*Series) error {
	return self.writeSeriesCommon(series, nil)
}

func (self *Client) WriteSeriesOverUDP(series []*Series) error {
	data, err := json.Marshal(series)
	if err != nil {
		return err
	}
	// because max of msg over upd is 2048 bytes
	// https://github.com/influxdb/influxdb/blob/master/src/api/udp/api.go#L65
	if len(data) >= UDPMaxMessageSize {
		err = fmt.Errorf("data size over limit %v limit is %v", len(data), UDPMaxMessageSize)
		fmt.Println(err)
		return err
	}
	_, err = self.udpConn.Write(data)
	if err != nil {
		return err
	}
	return nil
}

func (self *Client) WriteSeriesWithTimePrecision(series []*Series, timePrecision TimePrecision) error {
	return self.writeSeriesCommon(series, map[string]string{"time_precision": string(timePrecision)})
}

func (self *Client) writeSeriesCommon(series []*Series, options map[string]string) error {
	data, err := json.Marshal(series)
	if err != nil {
		return err
	}
	url := self.getUrl("/db/" + self.database + "/series")
	for name, value := range options {
		url += fmt.Sprintf("&%s=%s", name, value)
	}
	var b *bytes.Buffer
	if self.compression {
		b = bytes.NewBuffer(nil)
		w := gzip.NewWriter(b)
		if _, err := w.Write(data); err != nil {
			return err
		}
		w.Flush()
		w.Close()
	} else {
		b = bytes.NewBuffer(data)
	}
	req, err := http.NewRequest("POST", url, b)
	if err != nil {
		return err
	}
	if self.compression {
		req.Header.Set("Content-Encoding", "gzip")
	}
	resp, err := self.httpClient.Do(req)
	return responseToError(resp, err, true)
}

func (self *Client) Query(query string, precision ...TimePrecision) ([]*Series, error) {
	return self.queryCommon(query, false, precision...)
}

func (self *Client) QueryWithNumbers(query string, precision ...TimePrecision) ([]*Series, error) {
	return self.queryCommon(query, true, precision...)
}

func (self *Client) queryCommon(query string, useNumber bool, precision ...TimePrecision) ([]*Series, error) {
	escapedQuery := url.QueryEscape(query)
	url := self.getUrl("/db/" + self.database + "/series")
	if len(precision) > 0 {
		url += "&time_precision=" + string(precision[0])
	}
	url += "&q=" + escapedQuery
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, err
	}
	if !self.compression {
		req.Header.Set("Accept-Encoding", "identity")
	}
	resp, err := self.httpClient.Do(req)
	err = responseToError(resp, err, false)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	series := []*Series{}
	decoder := json.NewDecoder(bytes.NewBuffer(data))
	if useNumber {
		decoder.UseNumber()
	}
	err = decoder.Decode(&series)
	if err != nil {
		return nil, err
	}
	return series, nil
}

func (self *Client) Ping() error {
	url := self.getUrl("/ping")
	resp, err := self.httpClient.Get(url)
	return responseToError(resp, err, true)
}

func (self *Client) AuthenticateDatabaseUser(database, username, password string) error {
	url := self.getUrlWithUserAndPass(fmt.Sprintf("/db/%s/authenticate", database), username, password)
	resp, err := self.httpClient.Get(url)
	return responseToError(resp, err, true)
}

func (self *Client) AuthenticateClusterAdmin(username, password string) error {
	url := self.getUrlWithUserAndPass("/cluster_admins/authenticate", username, password)
	resp, err := self.httpClient.Get(url)
	return responseToError(resp, err, true)
}

func (self *Client) GetContinuousQueries() ([]map[string]interface{}, error) {
	url := self.getUrlWithUserAndPass(fmt.Sprintf("/db/%s/continuous_queries", self.database), self.username, self.password)
	return self.listSomething(url)
}

func (self *Client) DeleteContinuousQueries(id int) error {
	url := self.getUrlWithUserAndPass(fmt.Sprintf("/db/%s/continuous_queries/%d", self.database, id), self.username, self.password)
	resp, err := self.del(url)
	return responseToError(resp, err, true)
}

type LongTermShortTermShards struct {
	LongTerm  []*Shard `json:"longTerm"`
	ShortTerm []*Shard `json:"shortTerm"`
}

type Shard struct {
	Id        uint32   `json:"id"`
	EndTime   int64    `json:"endTime"`
	StartTime int64    `json:"startTime"`
	ServerIds []uint32 `json:"serverIds"`
}

func (self *Client) GetShards() (*LongTermShortTermShards, error) {
	url := self.getUrlWithUserAndPass("/cluster/shards", self.username, self.password)
	body, err := self.get(url)
	if err != nil {
		return nil, err
	}
	shards := &LongTermShortTermShards{}
	err = json.Unmarshal(body, shards)
	if err != nil {
		return nil, err
	}

	return shards, nil
}

func (self *Client) DropShard(id uint32, serverIds []uint32) error {
	url := self.getUrlWithUserAndPass(fmt.Sprintf("/cluster/shards/%d", id), self.username, self.password)
	ids := map[string][]uint32{"serverIds": serverIds}
	body, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	_, err = self.delWithBody(url, bytes.NewBuffer(body))
	return err
}
