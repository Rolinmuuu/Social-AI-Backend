package service

import (
	"reflect"

	"socialai/shared/model"

	"github.com/olivere/elastic/v7"
)

func getPostFromSearchResult(searchResult *elastic.SearchResult) []model.Post {
	var posts []model.Post
	var ptype model.Post
	for _, item := range searchResult.Each(reflect.TypeOf(ptype)) {
		if p, ok := item.(model.Post); ok && !p.Deleted {
			posts = append(posts, p)
		}
	}
	return posts
}

func getDeletedPostFromSearchResult(searchResult *elastic.SearchResult) []model.Post {
	var posts []model.Post
	var ptype model.Post
	for _, item := range searchResult.Each(reflect.TypeOf(ptype)) {
		if p, ok := item.(model.Post); ok {
			posts = append(posts, p)
		}
	}
	return posts
}

func getCommentFromSearchResult(searchResult *elastic.SearchResult) []model.Comment {
	var comments []model.Comment
	var ctype model.Comment
	for _, item := range searchResult.Each(reflect.TypeOf(ctype)) {
		if c, ok := item.(model.Comment); ok && !c.Deleted {
			comments = append(comments, c)
		}
	}
	return comments
}
