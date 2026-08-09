package event

// ExtractStateChanges 从 Value 的 _state_changes 字段提取 StateChange 列表。
func ExtractStateChanges(v Value) []StateChange {
	obj, ok := v.AsObject()
	if !ok {
		return nil
	}
	raw, ok := obj["_state_changes"]
	if !ok {
		return nil
	}
	arr, ok := raw.AsArray()
	if !ok {
		return nil
	}
	var result []StateChange
	for _, item := range arr {
		io, ok := item.AsObject()
		if !ok {
			continue
		}
		sc := StateChange{}
		if v, ok := io["subject_type"]; ok {
			if s, ok := v.AsString(); ok {
				sc.SubjectType = s
			}
		}
		if v, ok := io["subject_id"]; ok {
			if s, ok := v.AsString(); ok {
				sc.SubjectID = s
			}
		}
		if v, ok := io["op"]; ok {
			if s, ok := v.AsString(); ok {
				sc.Op = s
			}
		}
		if v, ok := io["path"]; ok {
			if s, ok := v.AsString(); ok {
				sc.Path = s
			}
		}
		if v, ok := io["before"]; ok {
			sc.Before = v
		}
		if v, ok := io["after"]; ok {
			sc.After = v
		}
		if v, ok := io["version"]; ok {
			if n, ok := v.AsInt(); ok {
				sc.Version = n
			}
		}
		if v, ok := io["metadata"]; ok {
			sc.Metadata = v
		}
		result = append(result, sc)
	}
	return result
}
