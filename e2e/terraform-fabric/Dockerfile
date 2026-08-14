# HashiCorp's published Terraform image, unmodified binary. The driver is a
# thin harness around `terraform init/apply/destroy`; nothing in Terraform
# or microsoft/fabric is patched. Pin a versioned tag — `latest` would make
# a Terraform release our flake.
FROM hashicorp/terraform:1.12.2
RUN apk add --no-cache python3 openssl ca-certificates \
    && python3 --version
WORKDIR /work
COPY main.tf /work/main.tf
COPY driver.py /driver.py
ENTRYPOINT ["python3", "/driver.py"]
