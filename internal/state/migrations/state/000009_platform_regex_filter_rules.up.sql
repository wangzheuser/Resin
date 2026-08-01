UPDATE platforms
SET regex_filters_json = (
	SELECT COALESCE(json_group_array('*' || value), '[]')
	FROM json_each(platforms.regex_filters_json)
);
