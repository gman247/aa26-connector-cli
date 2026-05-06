IMG := connector-prototype/demo-shell:dev

.PHONY: image import
image:
	sudo docker build -t $(IMG) .

import: image
	sudo docker save $(IMG) | sudo k3s ctr images import -
