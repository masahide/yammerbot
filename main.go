package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"regexp"
	"strings"

	"github.com/k0kubun/pp"
	"github.com/masahide/go-yammer/cometd"
	"github.com/masahide/go-yammer/schema"
	"github.com/masahide/go-yammer/yammer"
)

const (
	configFile = "cache.json"
)

var (
	conf        cache
	groupPrefix = byte('%')
	mentionRe   = regexp.MustCompile(`\[\[user:[\d]+\]\]`)
	tokenRe     = regexp.MustCompile(`\t+|\s+|"|,|\.|　+|\n+`)
	client      *yammer.Client
)

// MentionList is mention group
type MentionList struct {
	Name  string
	Users []string
}

type cache struct {
	AccessToken  string
	UserID       string
	MentionLists map[string]MentionList
}

func loadCache(file string) cache {
	var c cache
	f, err := os.Open(file)
	if err != nil {
		log.Fatal(err)
	}
	dec := json.NewDecoder(f)
	if err := dec.Decode(&c); err != nil {
		log.Fatal(err)
	}
	if c.MentionLists == nil {
		c.MentionLists = make(map[string]MentionList)
	}
	return c
}

func saveCache(file string, config cache) error {
	b, err := json.Marshal(config)
	if err != nil {
		log.Fatal(err)
	}
	return ioutil.WriteFile(file, b, 0600)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	conf = loadCache(configFile)
	client = yammer.New(conf.AccessToken)

	realtime, err := client.Realtime()
	if err != nil {
		log.Println(err)
		return
	}
	inbox, err := client.InboxFeedV2()
	if err != nil {
		log.Println(err)
		return
	}

	rt := cometd.New(realtime.RealtimeURI, realtime.AuthenticationToken)
	err = rt.Handshake()
	if err != nil {
		log.Println(err)
		return
	}

	rt.SubscribeToFeed(inbox.ChannelID)
	messageChan := make(chan *cometd.ConnectionResponse, 10)
	//messageChan := make(chan *schema.MessageFeed, 10)
	stopChan := make(chan bool, 1)

	log.Printf("Polling Realtime channelID: %v\n", inbox.ChannelID)
	go rt.Poll(messageChan, stopChan)
	for {
		select {
		case m := <-messageChan:
			if m.Channel == "/meta/connect" {
				continue
			}
			if m.Data.Type != "message" {
				log.Printf("Data.Type is not message. channel:%#v", m)
				continue
			}
			if m.Data.Feed == nil {
				log.Printf("Data.Feed is nil. channel:%#v", m)
				continue
			}
			receiveMessage(m.Data.Feed)
		}
	}
}

func receiveMessage(feed *schema.MessageFeed) {
	for _, mes := range feed.Messages {
		analysis(*mes)
	}
}

func analysis(mes schema.Message) {
	log.Println(mes.SenderId, mes.Body.Parsed)
	m := getMentions(mes.Body.Parsed)
	if m.ToMe {
		dispatcher(mes, m)
	}
	//re.FindStringSubmatch("mes.Body.Parsed")
}

type mentions struct {
	Users []string
	ToMe  bool
}

func getMentions(mes string) mentions {
	m := mentionRe.FindAllString(mes, -1)
	res := mentions{Users: make([]string, 0, len(m)), ToMe: false}
	for _, u := range m {
		if u == conf.UserID {
			res.ToMe = true
		} else {
			res.Users = append(res.Users, u)
		}
	}
	return res
}
func dispatcher(mes schema.Message, m mentions) {
	tokens := scan(mes.Body.Parsed)
	group := getGroup(tokens)
	f := getAcction(mes.Body.Parsed)
	if f == nil {
		log.Printf("unknown: %s", mes.Body.Parsed)
		return
	}
	f(group, mes, m)

}

func scan(mes string) []string {
	return tokenRe.Split(mes, -1)
}

func getGroup(tokens []string) string {
	for _, t := range tokens {
		if len(t) > 1 {
			if byte(t[0]) == groupPrefix {
				return t
			}
		}
	}
	return ""
}
func getAcction(mes string) func(string, schema.Message, mentions) error {
	switch {
	case strings.Contains(mes, "追加"):
		return add
	case strings.Contains(mes, "削除"):
		return del
	case strings.Contains(mes, "表示"):
		return show
	case strings.Contains(mes, "メンバーは"):
		return show
	case strings.Contains(mes, "メンション"):
		return cc
	case strings.Contains(mes, "めんしょん"):
		return cc
	case strings.Contains(mes, "ccして"):
		return cc
	case strings.Contains(mes, "CCして"):
		return cc
	}
	return nil
}

func add(group string, mes schema.Message, m mentions) error {
	log.Printf("add mention group:%v, %v", group, m)

	var body string
	list, ok := conf.MentionLists[group]
	if !ok {
		conf.MentionLists[group] = MentionList{
			Name:  group,
			Users: m.Users,
		}
		body = fmt.Sprintf("%s は存在しないので作成しました\n", group)
	} else {
		for _, u := range m.Users {
			list.Users = appendIfMissing(list.Users, u)
		}
		conf.MentionLists[group] = list
	}
	err := saveCache(configFile, conf)
	if err != nil {
		body += fmt.Sprintf("メンバー更新に失敗しました.\nerr:%s", err)
	} else {
		body += fmt.Sprintf("%s のメンバーは\n %v に更新されました", group, list.Users)
	}
	postMes := &yammer.CreateMessageParams{Body: body, RepliedToId: mes.ThreadId}
	_, err = client.PostMessage(postMes)
	return err
}

func del(group string, mes schema.Message, m mentions) error {
	log.Printf("del mention group:%v, %v", group, m)
	var body string
	list, ok := conf.MentionLists[group]
	if !ok {
		log.Printf("not exist group:%v, %v", group, m)
		body = fmt.Sprintf("%s は存在しません\n", group)
	} else {
		for _, u := range m.Users {
			list.Users = deleteIfExists(list.Users, u)
		}
		conf.MentionLists[group] = list
		err := saveCache(configFile, conf)
		if err != nil {
			body += fmt.Sprintf("メンバー更新に失敗しました.\nerr:%s", err)
		} else {
			body += fmt.Sprintf("%s のメンバーは\n %v に更新されました", group, list.Users)
		}
	}
	postMes := &yammer.CreateMessageParams{Body: body, RepliedToId: mes.ThreadId}
	_, err := client.PostMessage(postMes)
	return err
}
func show(group string, mes schema.Message, m mentions) error {
	log.Printf("show mention group:%v, %v", group, m)
	body := fmt.Sprintf("%s は存在しません", group)
	list, ok := conf.MentionLists[group]
	if ok {
		body = fmt.Sprintf("%s のメンバーは\n %v です", group, list.Users)
	}
	postMes := &yammer.CreateMessageParams{Body: body, RepliedToId: mes.ThreadId}
	_, err := client.PostMessage(postMes)
	return err
}

func cc(group string, mes schema.Message, m mentions) error {
	log.Printf("cc mention group:%v, %v", group, m)
	var ccIDs string
	body := fmt.Sprintf("%s は存在しません", group)
	list, ok := conf.MentionLists[group]
	if ok {
		body = fmt.Sprintf("%s にccします", group)
		ccIDs = strings.Join(list.Users, ",")
	}
	postMes := &yammer.CreateMessageParams{Body: body, RepliedToId: mes.ThreadId, CC: ccIDs}
	res, err := client.PostMessage(postMes)
	pp.Print(res, err)
	return err
}

func deleteIfExists(slice []string, s string) []string {
	res := make([]string, 0, len(slice))
	for _, ele := range slice {
		if ele == s {
			continue
		}
		res = append(res, ele)
	}
	return res
}
func appendIfMissing(slice []string, s string) []string {
	for _, ele := range slice {
		if ele == s {
			return slice
		}
	}
	return append(slice, s)
}
