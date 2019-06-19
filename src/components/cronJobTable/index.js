import React, { Component } from 'react';
// import logo from './logo.svg';
import { Layout, Menu, Icon, Table, Modal } from 'antd'
// import './App.css';
import 'whatwg-fetch'

const { Header, Sider, Content } = Layout


// var dataSource
class CronJobTable extends Component {

  state = {
    data: [],
    loading: false,
    visible: false,
    message: ''
  }
  componentDidMount() {
    this.fetchCronJob()
    window.setInterval(this.fetchCronJob, 5000)
  }


  fetchCronJob = () => {
    this.setState({ loading: true })
    fetch('http://ui-forecastjob-vip01.stg.fwmrm.net:3270/crons')
      .then(v => {
        return v.json()
      })
      .then(v => {
        v.sort((a, b) => {
          let na = a.name
          let nb = b.name
          if (na < nb) {
            return -1
          }
          if (na > nb) {
            return 1
          }
          return 0
        })
        this.setState({
          data: v,
          loading: false
        })
      })
  }

  deleteCronJob = (name) => {
    fetch('http://ui-forecastjob-vip01.stg.fwmrm.net:3270/removeCron', {
      method: 'POST',
      body: JSON.stringify({
        name: name
      })
    }).then(v => {
      return v.json()
    }).then((r) => {
      if (r.status === 'success') {
        this.fetchCronJob()
      } else {
        this.showModal(r.message)
      }
    })
  }

  showModal = (msg) => {
    this.setState({
      visible: true,
      message: msg
    });
  }

  handleOk = (e) => {
    console.log(e);
    this.setState({
      visible: false,
    });
  }

  handleCancel = (e) => {
    console.log(e);
    this.setState({
      visible: false,
    });
  }

  render() {
    const columns = [{
      title: 'Id',
      dataIndex: 'id',
      key: 'id',
    }, {
      title: 'Name',
      dataIndex: 'name',
      key: 'name',
    }, {
      title: 'Prev',
      dataIndex: 'prev',
      key: 'prev',
    }, {
      title: 'Next',
      dataIndex: 'next',
      key: 'next',
    }, {
      title: 'Spec',
      dataIndex: 'spec',
      key: 'spec',
    }, {
      title: 'Action',
      key: 'action',
      render: (text, record) => {
        console.log(record.name)
        return <span>
          <a onClick={()=>this.deleteCronJob(record.name)}>Delete</a>
        </span>
      }
    }];
    return (
      <Layout style={{ minHeight: '100vh'}} >
        <Layout>
          <Header style={{ background: '#fff', padding: 0 }} />
          <Content style={{ margin: '0 16px' }}>
          <Modal title="Error"
          visible={this.state.visible}
          onOk={this.handleOk}
          onCancel={this.handleCancel}
          >
          <p>{this.state.message}</p>
          </Modal>
            <Table dataSource={this.state.data} columns = {columns} loading={this.state.loading} />
          </Content>
        </Layout>
      </Layout>
    );
  }
}

export default CronJobTable;
