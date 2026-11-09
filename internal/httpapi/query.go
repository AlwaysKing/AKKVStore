package httpapi

import (
	"unicode/utf8"

	"akkvstore/internal/store"
)

type queryParams struct {
	Project        string                  `json:"project"`
	Tables         []string                `json:"tables,omitempty"`
	Condition      store.QueryConditionSet `json:"condition,omitempty"`
	IncludeDeleted bool                    `json:"includeDeleted,omitempty"`
	IncludeValues  bool                    `json:"includeValues,omitempty"`
	Limit          int                     `json:"limit,omitempty"`
	Offset         int                     `json:"offset,omitempty"`
}

type queryResponse struct {
	Items      []keyItem `json:"items"`
	Total      int64     `json:"total"`
	NextOffset int       `json:"nextOffset"`
	HasMore    bool      `json:"hasMore"`
}

func (a *API) queryRecords(p store.Project, params queryParams) (queryResponse, error) {
	result, err := a.store.QueryProjectRecords(p, store.AdvancedRecordQuery{
		Tables:         params.Tables,
		Condition:      params.Condition,
		IncludeDeleted: params.IncludeDeleted,
		IncludeValues:  params.IncludeValues,
		Limit:          params.Limit,
		Offset:         params.Offset,
	})
	if err != nil {
		return queryResponse{}, err
	}
	items := make([]keyItem, 0, len(result.Records))
	for _, projectRecord := range result.Records {
		record := projectRecord.Record
		item := keyItem{
			Table:       projectRecord.Table,
			Key:         record.Key,
			ContentType: record.ContentType,
			Size:        record.Size,
			UpdatedAt:   record.UpdatedAt,
			Deleted:     record.Deleted,
		}
		if record.ContentType == store.ValueTypeFile && !record.Deleted {
			item.ValueOmitted = true
		}
		if params.IncludeValues && !record.Deleted {
			switch record.ContentType {
			case store.ValueTypeString:
				if utf8.Valid(record.Value) {
					value := string(record.Value)
					item.Value = &value
				} else {
					item.ValueOmitted = true
				}
			}
		}
		items = append(items, item)
	}
	nextOffset := params.Offset + len(items)
	return queryResponse{
		Items:      items,
		Total:      result.Total,
		NextOffset: nextOffset,
		HasMore:    int64(nextOffset) < result.Total,
	}, nil
}
