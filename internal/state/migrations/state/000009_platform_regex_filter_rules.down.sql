UPDATE platforms
SET regex_filters_json = (
	SELECT COALESCE(
		json_group_array(
			CASE
				WHEN substr(value, 1, 1) = '*' THEN substr(value, 2)
				ELSE value
			END
		),
		'[]'
	)
	FROM json_each(platforms.regex_filters_json)
);
